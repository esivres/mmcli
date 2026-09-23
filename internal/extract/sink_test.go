package extract

import (
	"bytes"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The sink must return exactly the first and last bytes of any write
// sequence, whatever the chunking; this is what the reader sees.
func TestSinkKeepsHeadAndTail(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < 2000; iter++ {
		keep := 3 + rng.Intn(40)
		var all []byte
		s := newSink(keep, 0)
		for n := rng.Intn(20); n > 0; n-- {
			chunk := make([]byte, rng.Intn(30))
			for i := range chunk {
				chunk[i] = 'a' + byte(rng.Intn(26))
			}
			all = append(all, chunk...)
			_, _ = s.Write(chunk)
		}
		r := s.result(false)
		headMax, tailMax := keep*2/3, keep-keep*2/3
		if len(all) <= keep {
			if r.Text != string(all) || r.Truncated {
				t.Fatalf("short input altered: %q -> %q", all, r.Text)
			}
			continue
		}
		want := string(all[:headMax]) + "\n[... " + itoa(len(all)-keep) + " bytes omitted ...]\n" + string(all[len(all)-tailMax:])
		if r.Text != want || r.Omitted != int64(len(all)-keep) {
			t.Fatalf("keep %d, input %q:\n got %q\nwant %q", keep, all, r.Text, want)
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// Only a rune split at the cut is dropped; an invalid byte deep inside the
// kept head must not eat the head.
func TestSinkRuneSeams(t *testing.T) {
	s := newSink(30, 0)
	_, _ = s.Write([]byte("ab\xffcdefghijklmnopqrя" + strings.Repeat("x", 100) + "яz"))
	r := s.result(false)
	if !strings.HasPrefix(r.Text, "ab\uFFFDcdefghijklmnopqr\n[...") {
		t.Fatalf("head damaged: %q", r.Text)
	}
	if strings.ContainsRune(strings.SplitN(r.Text, "]\n", 2)[1], '\uFFFD') {
		t.Fatalf("tail starts inside a rune: %q", r.Text)
	}
}

// Writes stop at the cap; output exactly at the cap is complete, not stopped.
func TestSinkStopBoundary(t *testing.T) {
	s := newSink(100, 10)
	if n, err := s.Write([]byte("0123456789")); n != 10 || err != nil || s.failed() {
		t.Fatalf("write to exactly the cap: n=%d err=%v stopped=%v", n, err, s.failed())
	}
	if n, err := s.Write([]byte("x")); n != 0 || !errors.Is(err, errOutputLimit) || !s.failed() {
		t.Fatalf("write past the cap: n=%d err=%v", n, err)
	}
}

// Many cells in one row referencing one big shared string must not be joined
// in memory: a few KB of xlsx once meant gigabytes.
func TestXlsxWideRowBounded(t *testing.T) {
	var cells strings.Builder
	for i := 0; i < 2000; i++ {
		cells.WriteString(`<c t="s"><v>0</v></c>`)
	}
	data := zipOf(t, map[string]string{
		"xl/workbook.xml":            `<workbook xmlns:r="r"><sheets><sheet name="S" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships><Relationship Id="rId1" Target="worksheets/sheet1.xml"/></Relationships>`,
		"xl/sharedStrings.xml":       `<sst><si><t>` + strings.Repeat("y", 1<<20) + `</t></si></sst>`,
		"xl/worksheets/sheet1.xml":   `<worksheet><sheetData><row>` + cells.String() + `</row></sheetData></worksheet>`,
	})
	lim := DefaultLimits
	lim.MaxOutput = 4 << 20
	start := time.Now()
	r, err := File(ctx, write(t, "w.xlsx", data), "w.xlsx", "", lim)
	if err != nil || !strings.Contains(r.Text, "output limit") {
		t.Fatalf("err %v, stopped %v", err, strings.Contains(r.Text, "output limit"))
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("wide row was materialised")
	}
}

// A pdftotext that floods its output must be killed at the output cap.
func TestPDFFloodIsKilled(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "pdftotext")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexec yes flood\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	lim := DefaultLimits
	lim.MaxOutput = 1 << 20
	done := make(chan Result, 1)
	go func() {
		r, _ := File(ctx, write(t, "x.pdf", []byte("%PDF")), "x.pdf", "application/pdf", lim)
		done <- r
	}()
	select {
	case r := <-done:
		if !r.Truncated || !bytes.Contains([]byte(r.Text), []byte("output limit")) {
			t.Fatalf("flood not reported as stopped: %q", r.Text[:60])
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pdftotext flood not killed")
	}
}
