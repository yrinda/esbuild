package logging

// Logging is currently designed to look and feel like clang's error format.
// Errors are streamed asynchronously as they happen, each error contains the
// contents of the line with the error, and the error count is limited by
// default.

import (
	"fmt"
	"os"
	"strings"

	"github.com/evanw/esbuild/internal/ast"
)

type Log struct {
	msgs chan Msg
}

type LogLevel int8

const (
	LevelNone LogLevel = iota
	LevelInfo
	LevelWarning
	LevelError
)

type MsgKind uint8

const (
	Error MsgKind = iota
	Warning
)

type Msg struct {
	Source *Source
	Start  int32
	Length int32
	Text   string
	Kind   MsgKind
}

type Source struct {
	Index        uint32
	AbsolutePath string
	PrettyPath   string
	Contents     string
}

func (s *Source) TextForRange(r ast.Range) string {
	return s.Contents[r.Loc.Start : r.Loc.Start+r.Len]
}

func (s *Source) RangeOfString(loc ast.Loc) ast.Range {
	text := s.Contents[loc.Start:]
	if len(text) == 0 {
		return ast.Range{loc, 0}
	}

	quote := text[0]
	if quote == '"' || quote == '\'' {
		// Search for the matching quote character
		for i := 1; i < len(text); i++ {
			c := text[i]
			if c == quote {
				return ast.Range{loc, int32(i + 1)}
			} else if c == '\\' {
				i += 1
			}
		}
	}

	return ast.Range{loc, 0}
}

type stderrLogInfo struct {
	msgs             []Msg
	errors           int
	warnings         int
	errorLimitWasHit bool
}

func plural(prefix string, count int) string {
	if count == 1 {
		return fmt.Sprintf("%d %s", count, prefix)
	}
	return fmt.Sprintf("%d %ss", count, prefix)
}

func (counts stderrLogInfo) String() string {
	if counts.errors == 0 {
		if counts.warnings == 0 {
			return "no errors"
		} else {
			return plural("warning", counts.warnings)
		}
	} else {
		if counts.warnings == 0 {
			return plural("error", counts.errors)
		} else {
			return fmt.Sprintf("%s and %s",
				plural("warning", counts.warnings),
				plural("error", counts.errors))
		}
	}
}

type TerminalInfo struct {
	IsTTY           bool
	UseColorEscapes bool
	Width           int
}

func NewStderrLog(options StderrOptions) (Log, func() []Msg) {
	msgs := make(chan Msg)
	done := make(chan stderrLogInfo)
	log := Log{msgs}
	terminalInfo := GetTerminalInfo(os.Stderr)

	switch options.Color {
	case ColorNever:
		terminalInfo.UseColorEscapes = false
	case ColorAlways:
		terminalInfo.UseColorEscapes = SupportsColorEscapes
	}

	go func(msgs chan Msg, done chan stderrLogInfo) {
		result := stderrLogInfo{}
		for msg := range msgs {
			result.msgs = append(result.msgs, msg)

			// Be silent if we're past the limit so we don't flood the terminal
			if result.errorLimitWasHit {
				continue
			}

			switch msg.Kind {
			case Error:
				result.errors++
				if options.LogLevel <= LevelError {
					os.Stderr.WriteString(msg.String(options, terminalInfo))
				}
			case Warning:
				result.warnings++
				if options.LogLevel <= LevelWarning {
					os.Stderr.WriteString(msg.String(options, terminalInfo))
				}
			}

			// Silence further output if we reached the error limit
			if options.ErrorLimit != 0 && result.errors >= options.ErrorLimit {
				result.errorLimitWasHit = true
				if options.LogLevel <= LevelError {
					fmt.Fprintf(os.Stderr, "%s reached (disable error limit with --error-limit=0)\n", result.String())
				}
			}
		}
		done <- result
	}(msgs, done)

	return log, func() []Msg {
		close(log.msgs)
		result := <-done

		// Print out a summary if the error limit wasn't hit
		if !result.errorLimitWasHit && options.LogLevel <= LevelInfo && (result.warnings != 0 || result.errors != 0) {
			fmt.Fprintf(os.Stderr, "%s\n", result.String())
		}

		return result.msgs
	}
}

func PrintErrorToStderr(osArgs []string, text string) {
	options := StderrOptions{}

	// Implement a mini argument parser so these options always work even if we
	// haven't yet gotten to the general-purpose argument parsing code
	for _, arg := range osArgs {
		switch arg {
		case "--color=false":
			options.Color = ColorNever
		case "--color=true":
			options.Color = ColorAlways
		case "--log-level=info":
			options.LogLevel = LevelInfo
		case "--log-level=warning":
			options.LogLevel = LevelWarning
		case "--log-level=error":
			options.LogLevel = LevelError
		}
	}

	log, join := NewStderrLog(options)
	log.AddError(nil, ast.Loc{}, text)
	join()
}

func NewDeferLog() (Log, func() []Msg) {
	msgs := make(chan Msg)
	done := make(chan []Msg)
	log := Log{msgs}

	go func(msgs chan Msg, done chan []Msg) {
		var result []Msg
		for msg := range msgs {
			result = append(result, msg)
		}
		done <- result
	}(msgs, done)

	return log, func() []Msg {
		close(log.msgs)
		return <-done
	}
}

const colorReset = "\033[0m"
const colorRed = "\033[31m"
const colorGreen = "\033[32m"
const colorMagenta = "\033[35m"
const colorBold = "\033[1m"
const colorResetBold = "\033[0;1m"

type StderrColor uint8

const (
	ColorIfTerminal StderrColor = iota
	ColorNever
	ColorAlways
)

type StderrOptions struct {
	IncludeSource bool
	ErrorLimit    int
	Color         StderrColor
	LogLevel      LogLevel
}

func (msg Msg) String(options StderrOptions, terminalInfo TerminalInfo) string {
	kind := "error"
	kindColor := colorRed

	if msg.Kind == Warning {
		kind = "warning"
		kindColor = colorMagenta
	}

	if msg.Source == nil {
		if terminalInfo.UseColorEscapes {
			return fmt.Sprintf("%s%s%s: %s%s%s\n",
				colorBold, kindColor, kind,
				colorResetBold, msg.Text,
				colorReset)
		}

		return fmt.Sprintf("%s: %s\n", kind, msg.Text)
	}

	if !options.IncludeSource {
		if terminalInfo.UseColorEscapes {
			return fmt.Sprintf("%s%s: %s%s: %s%s%s\n",
				colorBold, msg.Source.PrettyPath,
				kindColor, kind,
				colorResetBold, msg.Text,
				colorReset)
		}

		return fmt.Sprintf("%s: %s: %s\n", msg.Source.PrettyPath, kind, msg.Text)
	}

	d := detailStruct(msg, terminalInfo)

	if terminalInfo.UseColorEscapes {
		return fmt.Sprintf("%s%s:%d:%d: %s%s: %s%s\n%s%s%s%s%s%s\n%s%s%s%s\n",
			colorBold, d.Path,
			d.Line,
			d.Column,
			kindColor, d.Kind,
			colorResetBold, d.Message,
			colorReset, d.SourceBefore, colorGreen, d.SourceMarked, colorReset, d.SourceAfter,
			colorGreen, d.Indent, d.Marker,
			colorReset)
	}

	return fmt.Sprintf("%s:%d:%d: %s: %s\n%s\n%s%s\n",
		d.Path, d.Line, d.Column, d.Kind, d.Message, d.Source, d.Indent, d.Marker)
}

type MsgDetail struct {
	Path    string
	Line    int
	Column  int
	Kind    string
	Message string

	// Source == SourceBefore + SourceMarked + SourceAfter
	Source       string
	SourceBefore string
	SourceMarked string
	SourceAfter  string

	Indent string
	Marker string
}

func ComputeLineAndColumn(text string) (lineCount int, columnCount, lastLineStart int) {
	var prevCodePoint rune

	for i, codePoint := range text {
		switch codePoint {
		case '\n':
			lastLineStart = i + 1
			if prevCodePoint != '\r' {
				lineCount++
			}
		case '\r', '\u2028', '\u2029':
			lastLineStart = i + 1
		}
		prevCodePoint = codePoint
	}

	columnCount = len(text) - lastLineStart
	return
}

func detailStruct(msg Msg, terminalInfo TerminalInfo) MsgDetail {
	contents := msg.Source.Contents
	lineCount, columnCount, lineStart := ComputeLineAndColumn(contents[0:msg.Start])
	lineEnd := len(contents)

loop:
	for i, codePoint := range contents[lineStart:] {
		switch codePoint {
		case '\r', '\n', '\u2028', '\u2029':
			lineEnd = lineStart + i
			break loop
		}
	}

	spacesPerTab := 2
	lineText := renderTabStops(contents[lineStart:lineEnd], spacesPerTab)
	indent := strings.Repeat(" ", len(renderTabStops(contents[lineStart:msg.Start], spacesPerTab)))
	marker := "^"
	markerStart := len(indent)
	markerEnd := len(indent)

	// Extend markers to cover the full range of the error
	if msg.Length > 0 {
		markerEnd = len(renderTabStops(contents[lineStart:msg.Start+msg.Length], spacesPerTab))
	}

	// Clip the marker to the bounds of the line
	if markerStart > len(lineText) {
		markerStart = len(lineText)
	}
	if markerEnd > len(lineText) {
		markerEnd = len(lineText)
	}
	if markerEnd < markerStart {
		markerEnd = markerStart
	}

	// Trim the line to fit the terminal width
	if terminalInfo.Width > 0 && len(lineText) > terminalInfo.Width {
		// Try to center the error
		sliceStart := (markerStart + markerEnd - terminalInfo.Width) / 2
		if sliceStart > markerStart-terminalInfo.Width/5 {
			sliceStart = markerStart - terminalInfo.Width/5
		}
		if sliceStart < 0 {
			sliceStart = 0
		}
		if sliceStart > len(lineText)-terminalInfo.Width {
			sliceStart = len(lineText) - terminalInfo.Width
		}
		sliceEnd := sliceStart + terminalInfo.Width

		// Slice the line
		slicedLine := lineText[sliceStart:sliceEnd]
		markerStart -= sliceStart
		markerEnd -= sliceStart
		if markerStart < 0 {
			markerStart = 0
		}
		if markerEnd > len(slicedLine) {
			markerEnd = len(slicedLine)
		}

		// Truncate the ends with "..."
		if len(slicedLine) > 3 && sliceStart > 0 {
			slicedLine = "..." + slicedLine[3:]
			if markerStart < 3 {
				markerStart = 3
			}
		}
		if len(slicedLine) > 3 && sliceEnd < len(lineText) {
			slicedLine = slicedLine[:len(slicedLine)-3] + "..."
			if markerEnd > len(slicedLine)-3 {
				markerEnd = len(slicedLine) - 3
			}
			if markerEnd < markerStart {
				markerEnd = markerStart
			}
		}

		// Now we can compute the indent
		indent = strings.Repeat(" ", markerStart)
		lineText = slicedLine
	}

	// If marker is still multi-character after clipping, make the marker wider
	if markerEnd-markerStart > 1 {
		marker = strings.Repeat("~", markerEnd-markerStart)
	}

	kind := "error"
	if msg.Kind == Warning {
		kind = "warning"
	}

	return MsgDetail{
		Path:    msg.Source.PrettyPath,
		Line:    lineCount + 1,
		Column:  columnCount,
		Kind:    kind,
		Message: msg.Text,

		Source:       lineText,
		SourceBefore: lineText[:markerStart],
		SourceMarked: lineText[markerStart:markerEnd],
		SourceAfter:  lineText[markerEnd:],

		Indent: indent,
		Marker: marker,
	}
}

func renderTabStops(withTabs string, spacesPerTab int) string {
	if !strings.ContainsRune(withTabs, '\t') {
		return withTabs
	}

	withoutTabs := strings.Builder{}
	count := 0

	for _, c := range withTabs {
		if c == '\t' {
			spaces := spacesPerTab - count%spacesPerTab
			for i := 0; i < spaces; i++ {
				withoutTabs.WriteRune(' ')
				count++
			}
		} else {
			withoutTabs.WriteRune(c)
			count++
		}
	}

	return withoutTabs.String()
}

func (log Log) AddError(source *Source, loc ast.Loc, text string) {
	log.msgs <- Msg{source, loc.Start, 0, text, Error}
}

func (log Log) AddWarning(source *Source, loc ast.Loc, text string) {
	log.msgs <- Msg{source, loc.Start, 0, text, Warning}
}

func (log Log) AddRangeError(source *Source, r ast.Range, text string) {
	log.msgs <- Msg{source, r.Loc.Start, r.Len, text, Error}
}

func (log Log) AddRangeWarning(source *Source, r ast.Range, text string) {
	log.msgs <- Msg{source, r.Loc.Start, r.Len, text, Warning}
}
