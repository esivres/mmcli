package extract

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var ctx = context.Background()

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
	r, err := File(ctx, write(t, "spec.docx", docx(t, `<w:p><w:r><w:t>Раздел</w:t><w:tab/><w:t>1</w:t></w:r></w:p><w:p><w:r><w:t>Текст</w:t></w:r></w:p>`)), "spec.docx", "", DefaultLimits)
	if err != nil || r.Text != "Раздел\t1\nТекст\n" {
		t.Fatalf("docx: %q, %v", r.Text, err)
	}
	r, err = File(ctx, write(t, "report.xlsx", xlsx(t)), "report.xlsx", "", DefaultLimits)
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
	r, err := File(ctx, write(t, "app.log", []byte(log)), "app.log", "", lim)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Truncated || !strings.HasPrefix(r.Text, "START") || !strings.HasSuffix(r.Text, "END") {
		t.Fatalf("truncation lost an end: %q", r.Text[:40])
	}
	if !strings.Contains(r.Text, fmt.Sprintf("[... %d bytes omitted ...]", r.Omitted)) || r.Omitted < int64(len(log)-300) {
		t.Fatalf("omitted %d of %d bytes: %q", r.Omitted, len(log), r.Text)
	}
	if !utf8Valid(r.Text) {
		t.Fatal("cut inside a character")
	}
}

func utf8Valid(s string) bool { return !strings.ContainsRune(s, '\uFFFD') }

// Images are refused explicitly (no OCR); unknown binaries too.
func TestRefusesImagesAndBinaries(t *testing.T) {
	if _, err := File(ctx, write(t, "shot.png", []byte("\x89PNG\r\n")), "shot.png", "image/png", DefaultLimits); !errors.Is(err, ErrImage) {
		t.Fatalf("png: %v", err)
	}
	if _, err := File(ctx, write(t, "blob.bin", []byte{0, 1, 2, 0}), "blob.bin", "application/octet-stream", DefaultLimits); err == nil {
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
	r, err := File(ctx, write(t, "a.zip", data), "a.zip", "", lim)
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
	r, _ = File(ctx, write(t, "a.zip", data), "a.zip", "", lim)
	if !strings.Contains(r.Text, "=== notes.txt ===\n(skipped: archive size limit reached)") {
		t.Errorf("total budget not enforced:\n%s", r.Text)
	}

	lim = DefaultLimits
	lim.MaxEntries = 2
	r, _ = File(ctx, write(t, "a.zip", data), "a.zip", "", lim)
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
	r, err := File(ctx, p, "guide.pdf", "application/pdf", DefaultLimits)
	if err != nil || !strings.Contains(r.Text, "Kafka incident guide") {
		t.Fatalf("pdf: %q, %v", r.Text, err)
	}
}

// Without pdftotext the caller must learn why, not get empty text.
func TestPDFWithoutTool(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := File(ctx, write(t, "guide.pdf", minimalPDF("x")), "guide.pdf", "application/pdf", DefaultLimits)
	if err == nil || !strings.Contains(err.Error(), "pdftotext not found") {
		t.Fatalf("want a missing-tool error, got %v", err)
	}
}

// A spoofed cell reference must not make the extractor allocate columns up
// to it: a few hundred bytes of xlsx once meant gigabytes of memory.
func TestXlsxHugeColumnRef(t *testing.T) {
	data := zipOf(t, map[string]string{
		"xl/workbook.xml":            `<workbook xmlns:r="r"><sheets><sheet name="S" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships><Relationship Id="rId1" Target="worksheets/sheet1.xml"/></Relationships>`,
		"xl/worksheets/sheet1.xml":   `<worksheet><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>a</t></is></c><c r="ZZZZZZZ1" t="inlineStr"><is><t>b</t></is></c><c r="C1"><v>c</v></c></row></sheetData></worksheet>`,
	})
	start := time.Now()
	r, err := File(ctx, write(t, "x.xlsx", data), "x.xlsx", "", DefaultLimits)
	if err != nil || r.Text != "== Sheet: S ==\na\tb\tc\n" {
		t.Fatalf("got %q, %v", r.Text, err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("huge column reference was expanded")
	}
}

// Sheets sharing one part, and one shared string referenced many times, must
// not multiply work beyond the limits.
func TestXlsxAmplificationBounded(t *testing.T) {
	var sheets, rels, cells strings.Builder
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&sheets, `<sheet name="S%d" r:id="rId1"/>`, i)
	}
	rels.WriteString(`<Relationships><Relationship Id="rId1" Target="worksheets/sheet1.xml"/></Relationships>`)
	for i := 0; i < 20000; i++ {
		cells.WriteString(`<row><c t="s"><v>0</v></c></row>`)
	}
	data := zipOf(t, map[string]string{
		"xl/workbook.xml":            `<workbook xmlns:r="r"><sheets>` + sheets.String() + `</sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": rels.String(),
		"xl/sharedStrings.xml":       `<sst><si><t>` + strings.Repeat("x", 1000) + `</t></si></sst>`,
		"xl/worksheets/sheet1.xml":   `<worksheet><sheetData>` + cells.String() + `</sheetData></worksheet>`,
	})
	lim := DefaultLimits
	lim.MaxOutput = 1 << 20
	r, err := File(ctx, write(t, "x.xlsx", data), "x.xlsx", "", lim)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Text, "extraction stopped at the 1048576-byte output limit") {
		t.Fatalf("output limit not applied: %d bytes", len(r.Text))
	}
	// The limit exists to stop the work, not just to label it.
	if processed := r.Omitted + int64(len(r.Text)); processed > 2<<20 {
		t.Fatalf("processed %d bytes past a 1 MiB limit", processed)
	}
	lim = DefaultLimits
	r, _ = File(ctx, write(t, "x.xlsx", data), "x.xlsx", "", lim)
	if strings.Count(r.Text, "(same data as an earlier sheet)") != 49 {
		t.Fatal("a sheet part shared by several sheets was re-read")
	}
}

// Formats are recognised by MIME too; zip-based types we cannot read are
// refused instead of being dumped as bytes.
func TestTypeDetection(t *testing.T) {
	d := docx(t, `<w:p><w:pPr><w:tabs><w:tab w:val="left"/></w:tabs></w:pPr><w:r><w:t>X</w:t></w:r></w:p>`+
		`<w:tbl><w:tr><w:tc><w:p><w:r><w:t>c1</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>c2</w:t></w:r></w:p></w:tc></w:tr></w:tbl>`)
	r, err := File(ctx, write(t, "noext", d), "noext", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", DefaultLimits)
	if err != nil || r.Text != "X\nc1 \tc2 \t\n" {
		t.Fatalf("docx by mime: %q, %v", r.Text, err)
	}
	pptx := zipOf(t, map[string]string{"ppt/slides/slide1.xml": "<p:sld/>"})
	if _, err := File(ctx, write(t, "deck.pptx", pptx), "deck.pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation", DefaultLimits); err == nil {
		t.Fatal("pptx dumped as text")
	}
}

// Out-of-order cells land in their own columns.
func TestXlsxCellPlacement(t *testing.T) {
	data := zipOf(t, map[string]string{
		"xl/workbook.xml":            `<workbook xmlns:r="r"><sheets><sheet name="S" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships><Relationship Id="rId1" Target="worksheets/sheet1.xml"/></Relationships>`,
		"xl/worksheets/sheet1.xml":   `<worksheet><sheetData><row r="2"><c r="C2"><v>c</v></c><c r="A2"><v>a</v></c><c r="AA2"><v>z</v></c></row></sheetData></worksheet>`,
	})
	r, err := File(ctx, write(t, "x.xlsx", data), "x.xlsx", "", DefaultLimits)
	want := "== Sheet: S ==\na\t\tc" + strings.Repeat("\t", 24) + "z\n" // AA is column 26
	if err != nil || r.Text != want {
		t.Fatalf("got %q want %q (%v)", r.Text, want, err)
	}
}

// An oversized entry still consumes the archive budget, so a zip of many
// such entries cannot read unbounded data.
func TestZipOversizedEntryChargesBudget(t *testing.T) {
	data := zipOf(t, map[string]string{"a.log": strings.Repeat("0", 3<<20), "b.txt": "after"})
	lim := DefaultLimits
	lim.MaxFileBytes = 1 << 20
	lim.MaxTotalBytes = int64(len(data)) + 1<<20
	r, err := File(ctx, write(t, "a.zip", data), "a.zip", "", lim)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Text, "=== b.txt ===\n(skipped: archive size limit reached)") {
		t.Fatalf("budget not charged for the oversized entry:\n%s", r.Text)
	}
}
