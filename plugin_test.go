package pdf_text_extractor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"
	"github.com/pocketbase/pocketbase/tools/types"
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

func TestInitCreatesPluginsCollection(t *testing.T) {
	app := newTestApp(t)

	p := &Plugin{}
	if err := p.Init(app); err != nil {
		t.Fatalf("failed to init plugin: %v", err)
	}

	collection, err := app.FindCollectionByNameOrId(pluginsCollectionName)
	if err != nil {
		t.Fatalf("expected %s collection to exist: %v", pluginsCollectionName, err)
	}

	if err := validatePluginsCollection(collection); err != nil {
		t.Fatalf("unexpected %s validation error: %v", pluginsCollectionName, err)
	}
}

func TestInitBeforeBootstrapDefersCollectionSetupUntilBootstrap(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "pb_data")
	app := core.NewBaseApp(core.BaseAppConfig{
		DataDir: dataDir,
	})

	p := &Plugin{}
	if err := p.Init(app); err != nil {
		t.Fatalf("failed to init plugin before bootstrap: %v", err)
	}

	if app.IsBootstrapped() {
		t.Fatal("did not expect app to be bootstrapped yet")
	}

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

	collection, err := app.FindCollectionByNameOrId(pluginsCollectionName)
	if err != nil {
		t.Fatalf("expected %s collection to exist after bootstrap: %v", pluginsCollectionName, err)
	}

	if err := validatePluginsCollection(collection); err != nil {
		t.Fatalf("unexpected %s validation error after bootstrap: %v", pluginsCollectionName, err)
	}
}

func TestPluginLoadsConfigRowsForMissingCollectionsWithoutFailing(t *testing.T) {
	app := newTestApp(t)

	p := &Plugin{}
	if err := p.Init(app); err != nil {
		t.Fatalf("failed to init plugin: %v", err)
	}

	createPluginConfigRecord(t, app, true, []ExtractPdfTextConfig{
		{
			CollectionName: "docs",
			InputField:     "pdfs",
			OutputField:    "extracted_text",
		},
	})

	configs := p.state.configsForCollection("docs")
	if len(configs) != 1 {
		t.Fatalf("expected config to be loaded for missing collection, got %d entries", len(configs))
	}
}

func TestPluginPocketBaseIntegrationWithPluginsTable(t *testing.T) {
	app := newTestApp(t)
	docs := createDocsCollection(t, app)

	p := &Plugin{}
	if err := p.Init(app); err != nil {
		t.Fatalf("failed to init plugin: %v", err)
	}

	createPluginConfigRecord(t, app, true, []ExtractPdfTextConfig{
		{
			CollectionName: docs.Name,
			InputField:     "pdfs",
			OutputField:    "extracted_text",
		},
	})

	expectedSingle := expectedFixtureText(t)

	record := createRecordWithPDF(t, app, docs)

	if got := record.GetString("extracted_text"); got != expectedSingle {
		t.Fatalf("expected in-memory record to be updated after create:\n got: %q\nwant: %q", got, expectedSingle)
	}

	created, err := app.FindRecordById(docs, record.Id)
	if err != nil {
		t.Fatalf("failed to reload created record: %v", err)
	}

	if got := created.GetString("extracted_text"); got != expectedSingle {
		t.Fatalf("unexpected extracted text after create:\n got: %q\nwant: %q", got, expectedSingle)
	}

	if got := len(created.GetStringSlice("pdfs")); got != 1 {
		t.Fatalf("unexpected file count after create: got %d want %d", got, 1)
	}

	pdf2, err := filesystem.NewFileFromPath(filepath.Join("testdata", "dummy.pdf"))
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

	updated, err := app.FindRecordById(docs, record.Id)
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

func TestPluginSkipsUnrelatedUpdates(t *testing.T) {
	app := newTestApp(t)
	docs := createDocsCollection(t, app)

	p := &Plugin{}
	if err := p.Init(app); err != nil {
		t.Fatalf("failed to init plugin: %v", err)
	}

	createPluginConfigRecord(t, app, true, []ExtractPdfTextConfig{
		{
			CollectionName: docs.Name,
			InputField:     "pdfs",
			OutputField:    "extracted_text",
		},
	})

	record := createRecordWithPDF(t, app, docs)
	expectedSingle := expectedFixtureText(t)

	originalExtract := extractText
	t.Cleanup(func() {
		extractText = originalExtract
	})

	calls := 0
	extractText = func(path string) (string, error) {
		calls++
		return originalExtract(path)
	}

	record.Set("title", "bulk edit should not reparse pdfs")
	if err := app.Save(record); err != nil {
		t.Fatalf("failed to save unrelated record update: %v", err)
	}

	if calls != 0 {
		t.Fatalf("expected unrelated update to skip pdf extraction, got %d extraction calls", calls)
	}

	if got := record.GetString("extracted_text"); got != expectedSingle {
		t.Fatalf("unexpected extracted text after unrelated update:\n got: %q\nwant: %q", got, expectedSingle)
	}
}

func TestPluginProcessesUpdateWhenAddingFirstPDFToEmptyField(t *testing.T) {
	app := newTestApp(t)
	docs := createDocsCollection(t, app)

	p := &Plugin{}
	if err := p.Init(app); err != nil {
		t.Fatalf("failed to init plugin: %v", err)
	}

	createPluginConfigRecord(t, app, true, []ExtractPdfTextConfig{
		{
			CollectionName: docs.Name,
			InputField:     "pdfs",
			OutputField:    "extracted_text",
		},
	})

	record := core.NewRecord(docs)
	record.Set("title", "starts empty")

	if err := app.Save(record); err != nil {
		t.Fatalf("failed to save empty record: %v", err)
	}

	if got := record.GetString("extracted_text"); got != "" {
		t.Fatalf("expected empty create to skip extraction, got %q", got)
	}

	pdfFile, err := filesystem.NewFileFromPath(filepath.Join("testdata", "dummy.pdf"))
	if err != nil {
		t.Fatalf("failed to create file upload from fixture: %v", err)
	}

	record.Set("pdfs+", []any{pdfFile})

	if err := app.Save(record); err != nil {
		t.Fatalf("failed to save record after adding first pdf: %v", err)
	}

	expectedSingle := expectedFixtureText(t)
	if got := record.GetString("extracted_text"); got != expectedSingle {
		t.Fatalf("expected update to process first added pdf:\n got: %q\nwant: %q", got, expectedSingle)
	}

	reloaded, err := app.FindRecordById(docs, record.Id)
	if err != nil {
		t.Fatalf("failed to reload updated record: %v", err)
	}

	if got := reloaded.GetString("extracted_text"); got != expectedSingle {
		t.Fatalf("unexpected extracted text after adding first pdf:\n got: %q\nwant: %q", got, expectedSingle)
	}
}

func TestPluginReloadsWhenConfigRowsChange(t *testing.T) {
	app := newTestApp(t)
	docs := createDocsCollection(t, app)

	p := &Plugin{}
	if err := p.Init(app); err != nil {
		t.Fatalf("failed to init plugin: %v", err)
	}

	configRow := createPluginConfigRecord(t, app, false, []ExtractPdfTextConfig{
		{
			CollectionName: docs.Name,
			InputField:     "pdfs",
			OutputField:    "extracted_text",
		},
	})

	record := createRecordWithPDF(t, app, docs)

	if got := record.GetString("extracted_text"); got != "" {
		t.Fatalf("expected disabled config to skip extraction, got %q", got)
	}

	configRow.Set(enabledField, true)
	if err := app.Save(configRow); err != nil {
		t.Fatalf("failed to enable config row: %v", err)
	}

	stillUnprocessed, err := app.FindRecordById(docs, record.Id)
	if err != nil {
		t.Fatalf("failed to reload original record: %v", err)
	}

	if got := stillUnprocessed.GetString("extracted_text"); got != "" {
		t.Fatalf("expected existing rows to remain untouched after enable, got %q", got)
	}

	expectedSingle := expectedFixtureText(t)

	pdf2, err := filesystem.NewFileFromPath(filepath.Join("testdata", "dummy.pdf"))
	if err != nil {
		t.Fatalf("failed to create file upload from fixture: %v", err)
	}

	stillUnprocessed.Set("pdfs+", []any{pdf2})
	if err := app.Save(stillUnprocessed); err != nil {
		t.Fatalf("failed to update record after enabling config: %v", err)
	}

	expectedMerged := expectedSingle + "\n---\n" + expectedSingle
	if got := stillUnprocessed.GetString("extracted_text"); got != expectedMerged {
		t.Fatalf("expected updated row to be processed after enable:\n got: %q\nwant: %q", got, expectedMerged)
	}

	if err := app.Delete(configRow); err != nil {
		t.Fatalf("failed to delete config row: %v", err)
	}

	processedRecord, err := app.FindRecordById(docs, record.Id)
	if err != nil {
		t.Fatalf("failed to reload processed record: %v", err)
	}

	processedRecord.Set("extracted_text", "")
	if err := app.Save(processedRecord); err != nil {
		t.Fatalf("failed to clear extracted text after config deletion: %v", err)
	}

	reloaded, err := app.FindRecordById(docs, record.Id)
	if err != nil {
		t.Fatalf("failed to reload record after config deletion: %v", err)
	}

	if got := reloaded.GetString("extracted_text"); got != "" {
		t.Fatalf("expected deleted config to stop future processing, got %q", got)
	}
}

func TestPluginRecalculatesExistingRowsWhenRequestedInConfig(t *testing.T) {
	app := newTestApp(t)
	docs := createDocsCollection(t, app)

	p := &Plugin{}
	if err := p.Init(app); err != nil {
		t.Fatalf("failed to init plugin: %v", err)
	}

	configRow := createPluginConfigRecord(t, app, false, []ExtractPdfTextConfig{
		{
			CollectionName: docs.Name,
			InputField:     "pdfs",
			OutputField:    "extracted_text",
		},
	})

	record1 := createRecordWithPDF(t, app, docs)
	record2 := createRecordWithPDF(t, app, docs)

	if got := record1.GetString("extracted_text"); got != "" {
		t.Fatalf("expected disabled config to skip extraction, got %q", got)
	}
	if got := record2.GetString("extracted_text"); got != "" {
		t.Fatalf("expected disabled config to skip extraction, got %q", got)
	}

	originalExtract := extractText
	t.Cleanup(func() {
		extractText = originalExtract
	})

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	extractText = func(path string) (string, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return originalExtract(path)
	}

	configs := mustParsePluginConfigs(t, configRow)
	configs[0].Recalculate = true
	setPluginConfigs(t, configRow, configs)
	configRow.Set(enabledField, true)
	if err := app.Save(configRow); err != nil {
		t.Fatalf("failed to request recalculation: %v", err)
	}

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for recalculation to start")
	}

	waitFor(t, 5*time.Second, func() bool {
		row, err := app.FindRecordById(pluginsCollectionName, configRow.Id)
		if err != nil {
			return false
		}

		cfgs := mustParsePluginConfigs(t, row)
		return len(cfgs) == 1 && !cfgs[0].Recalculate && cfgs[0].Recalculating
	})

	close(release)

	waitFor(t, 5*time.Second, func() bool {
		row, err := app.FindRecordById(pluginsCollectionName, configRow.Id)
		if err != nil {
			return false
		}

		cfgs := mustParsePluginConfigs(t, row)
		return len(cfgs) == 1 && !cfgs[0].Recalculate && !cfgs[0].Recalculating
	})

	expectedSingle := expectedFixtureText(t)

	waitFor(t, 5*time.Second, func() bool {
		reloaded1, err := app.FindRecordById(docs, record1.Id)
		if err != nil {
			return false
		}

		reloaded2, err := app.FindRecordById(docs, record2.Id)
		if err != nil {
			return false
		}

		return reloaded1.GetString("extracted_text") == expectedSingle &&
			reloaded2.GetString("extracted_text") == expectedSingle
	})
}

func TestPluginReloadsWhenCollectionsChange(t *testing.T) {
	app := newTestApp(t)

	p := &Plugin{}
	if err := p.Init(app); err != nil {
		t.Fatalf("failed to init plugin: %v", err)
	}

	createPluginConfigRecord(t, app, true, []ExtractPdfTextConfig{
		{
			CollectionName: "docs",
			InputField:     "pdfs",
			OutputField:    "extracted_text",
		},
	})

	docs := createDocsCollection(t, app)
	record := createRecordWithPDF(t, app, docs)
	expectedSingle := expectedFixtureText(t)

	if got := record.GetString("extracted_text"); got != expectedSingle {
		t.Fatalf("expected collection create to reload plugin config:\n got: %q\nwant: %q", got, expectedSingle)
	}
}

func TestHandleRecordEventContinuesPastBrokenConfig(t *testing.T) {
	app := newTestApp(t)
	docs := createDocsCollection(t, app)

	p := &Plugin{
		state: newPluginState((&Plugin{}).Name()),
	}
	p.state.byColl[docs.Name] = []ExtractPdfTextConfig{
		{
			CollectionName: docs.Name,
			InputField:     "missing_field",
			OutputField:    "extracted_text",
		},
		{
			CollectionName: docs.Name,
			InputField:     "pdfs",
			OutputField:    "extracted_text",
		},
	}

	record := createRecordWithPDF(t, app, docs)
	record.Set("extracted_text", "")

	event := &core.RecordEvent{
		App:     app,
		Context: context.Background(),
	}
	event.Record = record

	err := p.handleRecordEvent(event)
	if err == nil {
		t.Fatal("expected a joined error for the broken config")
	}

	expectedSingle := expectedFixtureText(t)
	if got := record.GetString("extracted_text"); got != expectedSingle {
		t.Fatalf("expected valid config to still run after broken config:\n got: %q\nwant: %q", got, expectedSingle)
	}
}

func createDocsCollection(t *testing.T, app *core.BaseApp) *core.Collection {
	t.Helper()

	collection := core.NewBaseCollection("docs")
	collection.Fields.Add(
		&core.FileField{Name: "pdfs", MaxSelect: 10},
		&core.TextField{Name: "title"},
		&core.TextField{Name: "extracted_text"},
	)

	if err := app.Save(collection); err != nil {
		t.Fatalf("failed to save collection: %v", err)
	}

	return collection
}

func createPluginConfigRecord(t *testing.T, app *core.BaseApp, enabled bool, configs []ExtractPdfTextConfig) *core.Record {
	t.Helper()

	pluginsCollection, err := app.FindCollectionByNameOrId(pluginsCollectionName)
	if err != nil {
		t.Fatalf("failed to load %s collection: %v", pluginsCollectionName, err)
	}

	raw, err := jsonRaw(configs)
	if err != nil {
		t.Fatalf("failed to encode config json: %v", err)
	}

	record := core.NewRecord(pluginsCollection)
	record.Set(pluginNameField, (&Plugin{}).Name())
	record.Set(configField, raw)
	record.Set(enabledField, enabled)

	if err := app.Save(record); err != nil {
		t.Fatalf("failed to save plugin config record: %v", err)
	}

	return record
}

func createRecordWithPDF(t *testing.T, app *core.BaseApp, collection *core.Collection) *core.Record {
	t.Helper()

	pdfFile, err := filesystem.NewFileFromPath(filepath.Join("testdata", "dummy.pdf"))
	if err != nil {
		t.Fatalf("failed to create file upload from fixture: %v", err)
	}

	record := core.NewRecord(collection)
	record.Set("pdfs", []any{pdfFile})

	if err := app.Save(record); err != nil {
		t.Fatalf("failed to save record: %v", err)
	}

	return record
}

func expectedFixtureText(t *testing.T) string {
	t.Helper()

	expectedSingleRaw, err := pdfToText(filepath.Join("testdata", "dummy.pdf"))
	if err != nil {
		t.Fatalf("failed to extract fixture text: %v", err)
	}

	expectedSingle := strings.TrimSpace(expectedSingleRaw)
	if expectedSingle == "" {
		t.Fatal("expected fixture PDF to produce extracted text")
	}

	return expectedSingle
}

func jsonRaw(v any) (types.JSONRaw, error) {
	bytes, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}

	return types.JSONRaw(bytes), nil
}

func mustParsePluginConfigs(t *testing.T, row *core.Record) []ExtractPdfTextConfig {
	t.Helper()

	cfgs, err := parsePluginConfigs(row)
	if err != nil {
		t.Fatalf("failed to parse plugin configs: %v", err)
	}

	return cfgs
}

func setPluginConfigs(t *testing.T, row *core.Record, configs []ExtractPdfTextConfig) {
	t.Helper()

	raw, err := jsonRaw(configs)
	if err != nil {
		t.Fatalf("failed to encode config json: %v", err)
	}

	row.Set(configField, raw)
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("timed out waiting for condition")
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
