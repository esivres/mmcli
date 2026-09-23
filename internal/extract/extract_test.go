package extract

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func write(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func docx(t *testing.T, body string) []byte {
	return zipOf(t, map[string]string{"word/document.xml": `<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` + body + `</w:body></w:document>`})
}

func xlsx(t *testing.T) []byte {
	return zipOf(t, map[string]string{
		"xl/workbook.xml": `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>
			<sheet name="Итоги" sheetId="1" r:id="rId2"/><sheet name="Raw" sheetId="2" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
			<Relationship Id="rId1" Target="worksheets/sheet1.xml"/><Relationship Id="rId2" Target="/xl/worksheets/sheet2.xml"/></Relationships>`,
		"xl/sharedStrings.xml":     `<sst><si><t>Сервис</t></si><si><r><t>crawl</t></r><r><t>er</t></r></si></sst>`,
		"xl/worksheets/sheet1.xml": `<worksheet><sheetData><row><c r="A1" t="inlineStr"><is><t>raw</t></is></c></row></sheetData></worksheet>`,
		"xl/worksheets/sheet2.xml": `<worksheet><sheetData>
			<row><c r="A1" t="s"><v>0</v></c><c r="C1"><v>42</v></c></row>
			<row><c r="A2" t="s"><v>1</v></c></row></sheetData></worksheet>`,
	})
}

// Office documents are the common attachments; their text must come out in
// reading order, with sheet order taken from the workbook, not file names.
func TestOfficeDocuments(t *testing.T) {
	r, err := File(write(t, "spec.docx", docx(t, `<w:p><w:r><w:t>Раздел</w:t><w:tab/><w:t>1</w:t></w:r></w:p><w:p><w:r><w:t>Текст</w:t></w:r></w:p>`)), "spec.docx", "", DefaultLimits)
	if err != nil || r.Text != "Раздел\t1\nТекст\n" {
		t.Fatalf("docx: %q, %v", r.Text, err)
	}
	r, err = File(write(t, "report.xlsx", xlsx(t)), "report.xlsx", "", DefaultLimits)
	want := "== Sheet: Итоги ==\nСервис\t\t42\ncrawler\n== Sheet: Raw ==\nraw\n"
	if err != nil || r.Text != want {
		t.Fatalf("xlsx:\n got %q\nwant %q (%v)", r.Text, want, err)
	}
}

// A long log keeps its start and end, and says how much is missing.
func TestTruncateKeepsBothEnds(t *testing.T) {
	log := "START\n" + strings.Repeat("я", 10_000) + "\nEND"
	lim := DefaultLimits
	lim.MaxText = 300
	r, err := File(write(t, "app.log", []byte(log)), "app.log", "", lim)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Truncated || !strings.HasPrefix(r.Text, "START") || !strings.HasSuffix(r.Text, "END") {
		t.Fatalf("truncation lost an end: %q", r.Text[:40])
	}
	if want := len([]rune(log)) - 300; r.Omitted != want || !strings.Contains(r.Text, fmt.Sprintf("[... %d characters omitted ...]", want)) {
		t.Fatalf("omitted %d, want %d", r.Omitted, want)
	}
}

// Images are refused explicitly (no OCR); unknown binaries too.
func TestRefusesImagesAndBinaries(t *testing.T) {
	if _, err := File(write(t, "shot.png", []byte("\x89PNG\r\n")), "shot.png", "image/png", DefaultLimits); !errors.Is(err, ErrImage) {
		t.Fatalf("png: %v", err)
	}
	if _, err := File(write(t, "blob.bin", []byte{0, 1, 2, 0}), "blob.bin", "application/octet-stream", DefaultLimits); err == nil {
		t.Fatal("binary accepted as text")
	}
}

// Archives are untrusted: every entry is accounted for, and size, count and
// depth limits hold even when an entry expands far beyond its compressed size.
func TestZipLimits(t *testing.T) {
	inner := zipOf(t, map[string]string{"deep.zip": string(zipOf(t, map[string]string{"x.txt": "too deep"}))})
	data := zipOf(t, map[string]string{
		"notes.txt": "hello",
		"spec.docx": string(docx(t, `<w:p><w:r><w:t>from docx</w:t></w:r></w:p>`)),
		"bomb.log":  strings.Repeat("0", 3<<20),
		"pic.png":   "\x89PNG",
		"inner.zip": string(inner),
	})
	lim := DefaultLimits
	lim.MaxFileBytes = 1 << 20
	lim.MaxDepth = 1
	r, err := File(write(t, "a.zip", data), "a.zip", "", lim)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"=== notes.txt ===\nhello",
		"=== spec.docx ===\nfrom docx",
		"=== bomb.log ===\n(skipped: larger than 1048576 bytes)",
		"=== pic.png ===\n(skipped: image files",
		"=== deep.zip ===\n(skipped: nested archive deeper than 1)",
	} {
		if !strings.Contains(r.Text, want) {
			t.Errorf("missing %q in:\n%s", want, r.Text)
		}
	}

	// The total budget is shared: once spent, later entries are skipped.
	lim = DefaultLimits
	lim.MaxFileBytes = 4 << 20
	lim.MaxTotalBytes = 3 << 20
	r, _ = File(write(t, "a.zip", data), "a.zip", "", lim)
	if !strings.Contains(r.Text, "=== notes.txt ===\n(skipped: archive size limit reached)") {
		t.Errorf("total budget not enforced:\n%s", r.Text)
	}

	lim = DefaultLimits
	lim.MaxEntries = 2
	r, _ = File(write(t, "a.zip", data), "a.zip", "", lim)
	if !strings.Contains(r.Text, "more entries skipped (limit 2)") {
		t.Errorf("entry limit not reported:\n%s", r.Text)
	}
}

// minimalPDF builds a one-page PDF showing text, with a valid xref table.
func minimalPDF(text string) []byte {
	stream := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", text)
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

func TestPDF(t *testing.T) {
	p := write(t, "guide.pdf", minimalPDF("Kafka incident guide"))
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext not installed; PDF extraction not exercised")
	}
	r, err := File(p, "guide.pdf", "application/pdf", DefaultLimits)
	if err != nil || !strings.Contains(r.Text, "Kafka incident guide") {
		t.Fatalf("pdf: %q, %v", r.Text, err)
	}
}

// Without pdftotext the caller must learn why, not get empty text.
func TestPDFWithoutTool(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := File(write(t, "guide.pdf", minimalPDF("x")), "guide.pdf", "application/pdf", DefaultLimits)
	if err == nil || !strings.Contains(err.Error(), "pdftotext not found") {
		t.Fatalf("want a missing-tool error, got %v", err)
	}
}
