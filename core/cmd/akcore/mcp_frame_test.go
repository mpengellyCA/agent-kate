package main

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestReadMCPLineResyncsPastOversizeFrame pins the audit F24 stdio item: an
// over-long line must cost exactly that line, not the rest of the session. The
// old bufio.Scanner loop ended silently on one — the bridge stayed alive and
// connected while never answering another tool call.
func TestReadMCPLineResyncsPastOversizeFrame(t *testing.T) {
	input := `{"id":1}` + "\n" +
		`{"pad":"` + strings.Repeat("x", maxMCPFrameBytes+1024) + `"}` + "\n" +
		`{"id":2}` + "\n"
	r := bufio.NewReaderSize(strings.NewReader(input), 64*1024)

	line, oversize, err := readMCPLine(r)
	if err != nil || oversize || string(line) != `{"id":1}` {
		t.Fatalf("first frame: line=%q oversize=%v err=%v", line, oversize, err)
	}

	line, oversize, err = readMCPLine(r)
	if err != nil {
		t.Fatalf("an oversize frame must not end the loop: %v", err)
	}
	if !oversize {
		t.Fatal("oversize frame was not reported as such")
	}
	if line != nil {
		t.Fatalf("oversize frame was returned as data (%d bytes)", len(line))
	}

	line, oversize, err = readMCPLine(r)
	if err != nil || oversize || string(line) != `{"id":2}` {
		t.Fatalf("the bridge went deaf after an oversize frame: line=%q oversize=%v err=%v",
			line, oversize, err)
	}

	if _, _, err = readMCPLine(r); !errors.Is(err, io.EOF) {
		t.Fatalf("end of stream reported as %v, want EOF", err)
	}
}

// A frame split across many buffer refills must still arrive whole: MCP tool
// results routinely exceed the 64 KiB read buffer.
func TestReadMCPLineAccumulatesLargeFrame(t *testing.T) {
	body := `{"pad":"` + strings.Repeat("y", 512*1024) + `"}`
	r := bufio.NewReaderSize(strings.NewReader(body+"\n"), 64*1024)
	line, oversize, err := readMCPLine(r)
	if err != nil || oversize {
		t.Fatalf("oversize=%v err=%v", oversize, err)
	}
	if string(line) != body {
		t.Fatalf("frame was truncated: %d bytes, want %d", len(line), len(body))
	}
}
