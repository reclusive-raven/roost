package roost

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"
)

const (
	callLimit   = 1 << 10 // a tool call's input
	resultLimit = 2 << 10 // a tool result
	titleLimit  = 80
)

// Message is one indexed turn. Role is user, assistant, tool_call or
// tool_result.
type Message struct {
	Seq  int    `json:"seq"`
	Role string `json:"role"`
	TS   string `json:"ts,omitempty"`
	Text string `json:"text"`
}

// Transcript is what the index keeps of one file. These formats change
// without notice, so a record not understood is counted, never an error.
type Transcript struct {
	Session string
	Project string
	CWD     string
	Title   string
	Started string
	Ended   string
	Skipped int
	Damaged int
	Texts   []Message
}

func (t *Transcript) add(role, ts, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if role == "user" && t.Title == "" {
		line, _, _ := strings.Cut(text, "\n")
		t.Title = clip(line, titleLimit)
	}
	t.Texts = append(t.Texts, Message{Seq: len(t.Texts), Role: role, TS: ts, Text: text})
}

func (t *Transcript) stamp(ts string) {
	if ts == "" {
		return
	}
	if t.Started == "" {
		t.Started = ts
	}
	t.Ended = ts
}

// clip cuts s to at most n bytes on a character boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// lines calls fn on each line. A file cut off mid-record is normal for a
// session that ended abruptly, so fn reports a bad line rather than failing.
func lines(r io.Reader, fn func([]byte)) error {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			fn(line)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// open reads a transcript, decompressing Codex's .zst files.
func open(r io.Reader, name string) (io.Reader, func(), error) {
	if !strings.HasSuffix(name, ".zst") {
		return r, func() {}, nil
	}
	zr, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, nil, err
	}
	return zr, zr.Close, nil
}

// parse reads one transcript of the given kind. A read error is a damaged
// file, returned with whatever came before it.
func parse(r io.Reader, name, kind string) (Transcript, error) {
	in, done, err := open(r, name)
	if err != nil {
		return Transcript{}, err
	}
	defer done()
	t := Transcript{Session: idOf(name)}
	if kind == "codex" {
		err = lines(in, t.codexLine)
	} else {
		t.Project = projectOf(strings.TrimPrefix(name, "claude/projects/"))
		err = lines(in, t.claudeLine)
	}
	return t, err
}

var reminders = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

type claudeRecord struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	CWD       string `json:"cwd"`
	Title     string `json:"customTitle"`
	Message   *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type claudeBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
}

func (t *Transcript) claudeLine(line []byte) {
	var r claudeRecord
	if err := json.Unmarshal(line, &r); err != nil {
		t.Damaged++
		return
	}
	switch r.Type {
	case "user", "assistant":
		if r.Message == nil {
			t.Skipped++
			return
		}
		if t.CWD == "" {
			t.CWD = r.CWD
		}
		t.stamp(r.Timestamp)
		t.claudeContent(r.Type, r.Timestamp, r.Message.Content)
	case "custom-title":
		t.Title = clip(r.Title, titleLimit)
	default:
		t.Skipped++
	}
}

func (t *Transcript) claudeContent(role, ts string, raw json.RawMessage) {
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		t.add(role, ts, reminders.ReplaceAllString(plain, ""))
		return
	}
	var blocks []claudeBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Skipped++
		return
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			t.add(role, ts, reminders.ReplaceAllString(b.Text, ""))
		case "tool_use":
			t.add("tool_call", ts, b.Name+" "+clip(string(b.Input), callLimit))
		case "tool_result":
			t.add("tool_result", ts, clip(reminders.ReplaceAllString(flatText(b.Content), ""), resultLimit))
		default:
			t.Skipped++
		}
	}
}

// flatText reads a value that is a string or a list of text blocks.
func flatText(raw json.RawMessage) string {
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		return plain
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n")
}

type codexRecord struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

type codexPayload struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	CWD       string          `json:"cwd"`
	Name      string          `json:"name"`
	Input     string          `json:"input"`
	Arguments string          `json:"arguments"`
	Content   json.RawMessage `json:"content"`
	Output    json.RawMessage `json:"output"`
}

// injected are user messages Codex writes itself, ahead of the real prompt.
var injected = []string{"# AGENTS.md instructions", "<environment_context", "<user_instructions", "<permissions"}

func (t *Transcript) codexLine(line []byte) {
	var r codexRecord
	if err := json.Unmarshal(line, &r); err != nil {
		t.Damaged++
		return
	}
	var p codexPayload
	if r.Type == "session_meta" || r.Type == "response_item" {
		if err := json.Unmarshal(r.Payload, &p); err != nil {
			t.Damaged++
			return
		}
	}
	switch {
	case r.Type == "session_meta":
		t.CWD, t.Project = p.CWD, p.CWD
		t.stamp(r.Timestamp)
	case r.Type != "response_item":
		t.Skipped++
	case p.Type == "message" && (p.Role == "user" || p.Role == "assistant"):
		t.stamp(r.Timestamp)
		text := flatText(p.Content)
		if p.Role == "user" {
			for _, prefix := range injected {
				if strings.HasPrefix(strings.TrimSpace(text), prefix) {
					return
				}
			}
		}
		t.add(p.Role, r.Timestamp, text)
	case p.Type == "custom_tool_call":
		t.add("tool_call", r.Timestamp, p.Name+" "+clip(p.Input, callLimit))
	case p.Type == "function_call":
		t.add("tool_call", r.Timestamp, p.Name+" "+clip(p.Arguments, callLimit))
	case p.Type == "custom_tool_call_output" || p.Type == "function_call_output":
		t.add("tool_result", r.Timestamp, clip(flatText(p.Output), resultLimit))
	default:
		t.Skipped++
	}
}

// codexMeta reads a Codex file's first record, which names its directory.
func codexMeta(path string) (codexPayload, error) {
	f, err := os.Open(path)
	if err != nil {
		return codexPayload{}, err
	}
	defer func() { _ = f.Close() }()
	in, done, err := open(f, path)
	if err != nil {
		return codexPayload{}, err
	}
	defer done()
	line, err := bufio.NewReaderSize(in, 1<<16).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return codexPayload{}, err
	}
	var r codexRecord
	if err := json.Unmarshal(line, &r); err != nil {
		return codexPayload{}, err
	}
	var p codexPayload
	return p, json.Unmarshal(r.Payload, &p)
}
