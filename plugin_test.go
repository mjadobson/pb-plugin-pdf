package pdf_text_extractor

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"
)

func TestMergePDFTextsFiltersAndMergesPDFs(t *testing.T) {
	var gotPaths []string

	result := mergePDFTexts(
		[]string{"first.pdf", "ignore.txt", "SECOND.PDF"},
		func(name string) string {
			return filepath.Join("/tmp", name)
		},
		func(path string) (string, error) {
			gotPaths = append(gotPaths, path)
			switch filepath.Base(path) {
			case "first.pdf":
				return " First page text ", nil
			case "SECOND.PDF":
				return "Second page text", nil
			default:
				t.Fatalf("unexpected extraction path: %s", path)
				return "", nil
			}
		},
		nil,
	)

	wantPaths := []string{"/tmp/first.pdf", "/tmp/SECOND.PDF"}
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Fatalf("unexpected extracted paths: got %v want %v", gotPaths, wantPaths)
	}

	want := "First page text\n---\nSecond page text"
	if result != want {
		t.Fatalf("unexpected merged text:\n got: %q\nwant: %q", result, want)
	}
}

func TestMergePDFTextsSkipsFailuresAndBlankResults(t *testing.T) {
	var logged []string

	result := mergePDFTexts(
		[]string{"broken.pdf", "blank.pdf", "working.pdf"},
		func(name string) string { return name },
		func(path string) (string, error) {
			switch path {
			case "broken.pdf":
				return "", errors.New("boom")
			case "blank.pdf":
				return "   \n\t ", nil
			case "working.pdf":
				return "useful text", nil
			default:
				t.Fatalf("unexpected extraction path: %s", path)
				return "", nil
			}
		},
		func(name string, err error) {
			logged = append(logged, name+":"+err.Error())
		},
	)

	if result != "useful text" {
		t.Fatalf("unexpected merged text: got %q want %q", result, "useful text")
	}

	wantLogged := []string{"broken.pdf:boom"}
	if !reflect.DeepEqual(logged, wantLogged) {
		t.Fatalf("unexpected logged errors: got %v want %v", logged, wantLogged)
	}
}

func TestPluginMetadata(t *testing.T) {
	p := &Plugin{}

	if p.Name() != "pdf_text_extractor" {
		t.Fatalf("unexpected plugin name: %q", p.Name())
	}

	if p.Description() == "" {
		t.Fatal("expected plugin description to be non-empty")
	}

	if p.Version() == "" {
		t.Fatal("expected plugin version to be non-empty")
	}
}

func TestInitDoesNotFailWhenCollectionIsMissing(t *testing.T) {
	app := newTestApp(t)

	p := &Plugin{
		Configs: []ExtractPdfTextConfig{
			{
				CollectionName: "docs",
				InputField:     "pdfs",
				OutputField:    "extracted_text",
			},
		},
	}

	if err := p.Init(app); err != nil {
		t.Fatalf("expected init to tolerate missing collection, got error: %v", err)
	}
}

func TestInitFailsForInvalidExistingOutputFieldType(t *testing.T) {
	app := newTestApp(t)

	collection := core.NewBaseCollection("docs")
	collection.Fields.Add(
		&core.FileField{Name: "pdfs", MaxSelect: 10},
		&core.NumberField{Name: "extracted_text"},
	)

	if err := app.Save(collection); err != nil {
		t.Fatalf("failed to save collection: %v", err)
	}

	p := &Plugin{
		Configs: []ExtractPdfTextConfig{
			{
				CollectionName: "docs",
				InputField:     "pdfs",
				OutputField:    "extracted_text",
			},
		},
	}

	err := p.Init(app)
	if err == nil {
		t.Fatal("expected init to fail for non-text output field")
	}

	if !strings.Contains(err.Error(), "must be a text or editor field") {
		t.Fatalf("unexpected init error: %v", err)
	}
}

func TestPluginPocketBaseIntegrationWithRealPDF(t *testing.T) {
	app := newTestApp(t)

	collection := core.NewBaseCollection("docs")
	collection.Fields.Add(
		&core.FileField{Name: "pdfs", MaxSelect: 10},
		&core.TextField{Name: "extracted_text"},
	)

	if err := app.Save(collection); err != nil {
		t.Fatalf("failed to save collection: %v", err)
	}

	p := &Plugin{
		Configs: []ExtractPdfTextConfig{
			{
				CollectionName: "docs",
				InputField:     "pdfs",
				OutputField:    "extracted_text",
			},
		},
	}

	if err := p.Init(app); err != nil {
		t.Fatalf("failed to init plugin: %v", err)
	}

	pdfFixturePath := filepath.Join("testdata", "dummy.pdf")
	expectedSingleRaw, err := pdfToText(pdfFixturePath)
	if err != nil {
		t.Fatalf("failed to extract fixture text: %v", err)
	}
	expectedSingle := strings.TrimSpace(expectedSingleRaw)
	if expectedSingle == "" {
		t.Fatal("expected fixture PDF to produce extracted text")
	}

	pdf1, err := filesystem.NewFileFromPath(pdfFixturePath)
	if err != nil {
		t.Fatalf("failed to create file upload from fixture: %v", err)
	}

	record := core.NewRecord(collection)
	record.Set("pdfs", []any{pdf1})

	if err := app.Save(record); err != nil {
		t.Fatalf("failed to save record: %v", err)
	}

	if got := record.GetString("extracted_text"); got != expectedSingle {
		t.Fatalf("expected in-memory record to be updated after create:\n got: %q\nwant: %q", got, expectedSingle)
	}

	created, err := app.FindRecordById("docs", record.Id)
	if err != nil {
		t.Fatalf("failed to reload created record: %v", err)
	}

	if got := created.GetString("extracted_text"); got != expectedSingle {
		t.Fatalf("unexpected extracted text after create:\n got: %q\nwant: %q", got, expectedSingle)
	}

	if got := len(created.GetStringSlice("pdfs")); got != 1 {
		t.Fatalf("unexpected file count after create: got %d want %d", got, 1)
	}

	pdf2, err := filesystem.NewFileFromPath(pdfFixturePath)
	if err != nil {
		t.Fatalf("failed to create second file upload from fixture: %v", err)
	}

	txt, err := filesystem.NewFileFromBytes([]byte("ignore me"), "note.txt")
	if err != nil {
		t.Fatalf("failed to create text upload fixture: %v", err)
	}

	created.Set("pdfs+", []any{pdf2, txt})

	expectedMerged := expectedSingle + "\n---\n" + expectedSingle

	if err := app.Save(created); err != nil {
		t.Fatalf("failed to update record: %v", err)
	}

	if got := created.GetString("extracted_text"); got != expectedMerged {
		t.Fatalf("expected in-memory record to be updated after update:\n got: %q\nwant: %q", got, expectedMerged)
	}

	updated, err := app.FindRecordById("docs", record.Id)
	if err != nil {
		t.Fatalf("failed to reload updated record: %v", err)
	}

	if got := updated.GetString("extracted_text"); got != expectedMerged {
		t.Fatalf("unexpected extracted text after update:\n got: %q\nwant: %q", got, expectedMerged)
	}

	if got := len(updated.GetStringSlice("pdfs")); got != 3 {
		t.Fatalf("unexpected file count after update: got %d want %d", got, 3)
	}
}

func newTestApp(t *testing.T) *core.BaseApp {
	t.Helper()

	dataDir := filepath.Join(t.TempDir(), "pb_data")
	app := core.NewBaseApp(core.BaseAppConfig{
		DataDir: dataDir,
	})

	if err := app.Bootstrap(); err != nil {
		t.Fatalf("failed to bootstrap pocketbase app: %v", err)
	}

	t.Cleanup(func() {
		if err := app.ResetBootstrapState(); err != nil {
			t.Fatalf("failed to reset pocketbase app: %v", err)
		}

		if err := os.RemoveAll(dataDir); err != nil {
			t.Fatalf("failed to remove pocketbase data dir: %v", err)
		}
	})

	return app
}
