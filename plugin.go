package pdf_text_extractor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/moolekkari/unipdf/extractor"
	pdf "github.com/moolekkari/unipdf/model"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
	"github.com/pocketbuilds/xpb"
)

var version = "dev"
var extractText = pdfToText

const extractionInProgressKey = "@pdf_text_extractor_in_progress"
const pluginsCollectionName = "_plugins"
const pluginNameField = "plugin_name"
const configField = "config"
const enabledField = "enabled"

type ExtractPdfTextConfig struct {
	CollectionName string `json:"collection_name"`
	InputField     string `json:"input_field"`
	OutputField    string `json:"output_field"`
}

type Plugin struct {
	state *pluginState
}

type pluginState struct {
	pluginName string
	mu         sync.RWMutex
	byColl     map[string][]ExtractPdfTextConfig
}

func newPluginState(pluginName string) *pluginState {
	return &pluginState{
		pluginName: pluginName,
		byColl:     map[string][]ExtractPdfTextConfig{},
	}
}

func init() {
	xpb.Register(&Plugin{})
}

func (p *Plugin) Name() string {
	return "pdf_text_extractor"
}

func (p *Plugin) Version() string {
	return version
}

func (p *Plugin) Description() string {
	return "Extracts text from configured PDF file fields and writes the merged text into output fields."
}

func (p *Plugin) Init(app core.App) error {
	p.state = newPluginState(p.Name())

	if err := p.refreshState(app); err != nil {
		return err
	}

	app.OnRecordAfterCreateSuccess().BindFunc(func(e *core.RecordEvent) error {
		if err := p.handleRecordEvent(e); err != nil {
			e.App.Logger().Error(
				"pdf_text_extractor create hook failed",
				slog.String("collection", e.Record.Collection().Name),
				slog.String("recordId", e.Record.Id),
				slog.Any("error", err),
			)
		}

		return e.Next()
	})

	app.OnRecordAfterUpdateSuccess().BindFunc(func(e *core.RecordEvent) error {
		if err := p.handleRecordEvent(e); err != nil {
			e.App.Logger().Error(
				"pdf_text_extractor update hook failed",
				slog.String("collection", e.Record.Collection().Name),
				slog.String("recordId", e.Record.Id),
				slog.Any("error", err),
			)
		}

		return e.Next()
	})

	refreshConfigs := func(e *core.RecordEvent) error {
		if err := p.refreshState(e.App); err != nil {
			e.App.Logger().Error(
				"pdf_text_extractor config reload failed",
				slog.String("collection", e.Record.Collection().Name),
				slog.String("recordId", e.Record.Id),
				slog.Any("error", err),
			)
		}

		return e.Next()
	}

	app.OnRecordAfterCreateSuccess(pluginsCollectionName).BindFunc(refreshConfigs)
	app.OnRecordAfterUpdateSuccess(pluginsCollectionName).BindFunc(refreshConfigs)
	app.OnRecordAfterDeleteSuccess(pluginsCollectionName).BindFunc(refreshConfigs)

	refreshOnCollectionChange := func(e *core.CollectionEvent) error {
		if err := p.refreshState(e.App); err != nil {
			e.App.Logger().Error(
				"pdf_text_extractor config reload failed",
				slog.String("collection", e.Collection.Name),
				slog.Any("error", err),
			)
		}

		return e.Next()
	}

	app.OnCollectionAfterCreateSuccess().BindFunc(refreshOnCollectionChange)
	app.OnCollectionAfterUpdateSuccess().BindFunc(refreshOnCollectionChange)
	app.OnCollectionAfterDeleteSuccess().BindFunc(refreshOnCollectionChange)

	return nil
}

func (p *Plugin) handleRecordEvent(e *core.RecordEvent) error {
	if p.state == nil || e.Record.Collection().Name == pluginsCollectionName {
		return nil
	}

	configs := p.state.configsForCollection(e.Record.Collection().Name)
	var errs []error

	for _, cfg := range configs {
		if err := processRecord(e.Context, e.App, cfg, e.Record); err != nil {
			errs = append(errs, err)
			e.App.Logger().Error(
				"pdf_text_extractor config processing failed",
				slog.String("collection", cfg.CollectionName),
				slog.String("recordId", e.Record.Id),
				slog.String("inputField", cfg.InputField),
				slog.String("outputField", cfg.OutputField),
				slog.Any("error", err),
			)
		}
	}

	return errors.Join(errs...)
}

func (p *Plugin) refreshState(app core.App) error {
	if err := ensurePluginsCollection(app); err != nil {
		return err
	}

	return p.state.reload(app)
}

func (s *pluginState) reload(app core.App) error {
	rows, err := app.FindAllRecords(
		pluginsCollectionName,
		dbx.HashExp{
			pluginNameField: s.pluginName,
			enabledField:    true,
		},
	)
	if err != nil {
		return fmt.Errorf("load plugin configs: %w", err)
	}

	next := make(map[string][]ExtractPdfTextConfig)

	for _, row := range rows {
		cfgs, err := parsePluginConfigs(row)
		if err != nil {
			app.Logger().Error(
				"pdf_text_extractor invalid config row",
				slog.String("recordId", row.Id),
				slog.Any("error", err),
			)
			continue
		}

		for i, cfg := range cfgs {
			if err := validateConfigShape(cfg); err != nil {
				app.Logger().Error(
					"pdf_text_extractor invalid config entry",
					slog.String("recordId", row.Id),
					slog.Int("configIndex", i),
					slog.Any("error", err),
				)
				continue
			}

			collection, err := app.FindCollectionByNameOrId(cfg.CollectionName)
			if err != nil {
				app.Logger().Warn(
					"pdf_text_extractor config warning",
					slog.String("recordId", row.Id),
					slog.String("collection", cfg.CollectionName),
					slog.Any("error", fmt.Errorf("collection %q not found yet: %w", cfg.CollectionName, err)),
				)
				next[cfg.CollectionName] = append(next[cfg.CollectionName], cfg)
				continue
			}

			if err := validateConfigForCollection(cfg, collection); err != nil {
				app.Logger().Error(
					"pdf_text_extractor invalid collection config",
					slog.String("recordId", row.Id),
					slog.Int("configIndex", i),
					slog.String("collection", cfg.CollectionName),
					slog.Any("error", err),
				)
				continue
			}

			next[cfg.CollectionName] = append(next[cfg.CollectionName], cfg)
		}
	}

	s.mu.Lock()
	s.byColl = next
	s.mu.Unlock()

	return nil
}

func (s *pluginState) configsForCollection(collectionName string) []ExtractPdfTextConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()

	configs := s.byColl[collectionName]
	if len(configs) == 0 {
		return nil
	}

	cloned := make([]ExtractPdfTextConfig, len(configs))
	copy(cloned, configs)
	return cloned
}

func ensurePluginsCollection(app core.App) error {
	collection, err := app.FindCollectionByNameOrId(pluginsCollectionName)
	if err == nil {
		return validatePluginsCollection(collection)
	}

	collection = core.NewBaseCollection(pluginsCollectionName)
	collection.Fields.Add(
		&core.TextField{Name: pluginNameField, Required: true},
		&core.JSONField{Name: configField, Required: true},
		&core.BoolField{Name: enabledField},
	)

	if err := app.Save(collection); err != nil {
		return fmt.Errorf("create %s collection: %w", pluginsCollectionName, err)
	}

	return nil
}

func validatePluginsCollection(collection *core.Collection) error {
	pluginName := collection.Fields.GetByName(pluginNameField)
	if pluginName == nil {
		return fmt.Errorf("%s collection missing %q field", pluginsCollectionName, pluginNameField)
	}
	if _, ok := pluginName.(*core.TextField); !ok {
		return fmt.Errorf("%s.%s must be a text field", pluginsCollectionName, pluginNameField)
	}

	config := collection.Fields.GetByName(configField)
	if config == nil {
		return fmt.Errorf("%s collection missing %q field", pluginsCollectionName, configField)
	}
	if _, ok := config.(*core.JSONField); !ok {
		return fmt.Errorf("%s.%s must be a json field", pluginsCollectionName, configField)
	}

	enabled := collection.Fields.GetByName(enabledField)
	if enabled == nil {
		return fmt.Errorf("%s collection missing %q field", pluginsCollectionName, enabledField)
	}
	if _, ok := enabled.(*core.BoolField); !ok {
		return fmt.Errorf("%s.%s must be a bool field", pluginsCollectionName, enabledField)
	}

	return nil
}

func parsePluginConfigs(row *core.Record) ([]ExtractPdfTextConfig, error) {
	raw, ok := row.GetRaw(configField).(types.JSONRaw)
	if !ok {
		return nil, fmt.Errorf("config field is not json")
	}

	var configs []ExtractPdfTextConfig
	if err := json.Unmarshal(raw, &configs); err != nil {
		return nil, fmt.Errorf("decode config json: %w", err)
	}

	if len(configs) == 0 {
		return nil, errors.New("config must include at least one entry")
	}

	return configs, nil
}

func validateConfigShape(cfg ExtractPdfTextConfig) error {
	if cfg.CollectionName == "" || cfg.InputField == "" || cfg.OutputField == "" {
		return errors.New("config must include collection_name, input_field, and output_field")
	}

	return nil
}

func validateConfigForCollection(cfg ExtractPdfTextConfig, collection *core.Collection) error {
	input := collection.Fields.GetByName(cfg.InputField)
	if input == nil {
		return fmt.Errorf("input field %q not found in collection %q", cfg.InputField, cfg.CollectionName)
	}

	if _, ok := input.(*core.FileField); !ok {
		return fmt.Errorf("input field %q in collection %q must be a file field", cfg.InputField, cfg.CollectionName)
	}

	output := collection.Fields.GetByName(cfg.OutputField)
	if output == nil {
		return fmt.Errorf("output field %q not found in collection %q", cfg.OutputField, cfg.CollectionName)
	}

	if !isSupportedOutputField(output) {
		return fmt.Errorf("output field %q in collection %q must be a text or editor field", cfg.OutputField, cfg.CollectionName)
	}

	return nil
}

func isSupportedOutputField(field core.Field) bool {
	switch field.(type) {
	case *core.TextField, *core.EditorField:
		return true
	default:
		return false
	}
}

func processRecord(ctx context.Context, app core.App, cfg ExtractPdfTextConfig, record *core.Record) error {
	if inProgress, _ := record.GetRaw(extractionInProgressKey).(bool); inProgress {
		return nil
	}

	if err := validateRecordConfig(cfg, record); err != nil {
		return err
	}

	filenames := record.GetStringSlice(cfg.InputField)
	content := mergePDFTexts(
		filenames,
		func(name string) string {
			return filepath.Join(app.DataDir(), "storage", record.BaseFilesPath(), name)
		},
		extractText,
		func(name string, err error) {
			app.Logger().Error(
				"pdf_text_extractor failed to extract PDF",
				slog.String("collection", cfg.CollectionName),
				slog.String("recordId", record.Id),
				slog.String("file", name),
				slog.Any("error", err),
			)
		},
	)

	return updateOutputField(ctx, app, record, cfg.OutputField, content)
}

func validateRecordConfig(cfg ExtractPdfTextConfig, record *core.Record) error {
	input := record.Collection().Fields.GetByName(cfg.InputField)
	if input == nil {
		return fmt.Errorf("input field %q not found in collection %q", cfg.InputField, record.Collection().Name)
	}

	if _, ok := input.(*core.FileField); !ok {
		return fmt.Errorf("input field %q in collection %q must be a file field", cfg.InputField, record.Collection().Name)
	}

	output := record.Collection().Fields.GetByName(cfg.OutputField)
	if output == nil {
		return fmt.Errorf("output field %q not found in collection %q", cfg.OutputField, record.Collection().Name)
	}

	if !isSupportedOutputField(output) {
		return fmt.Errorf("output field %q in collection %q must be a text or editor field", cfg.OutputField, record.Collection().Name)
	}

	return nil
}

func mergePDFTexts(
	filenames []string,
	pathForFile func(string) string,
	extract func(string) (string, error),
	logError func(string, error),
) string {
	texts := make([]string, 0, len(filenames))

	for _, name := range filenames {
		if !strings.EqualFold(filepath.Ext(name), ".pdf") {
			continue
		}

		text, err := extract(pathForFile(name))
		if err != nil {
			if logError != nil {
				logError(name, err)
			}
			continue
		}

		trimmed := strings.TrimSpace(text)
		if trimmed != "" {
			texts = append(texts, trimmed)
		}
	}

	return strings.Join(texts, "\n---\n")
}

func updateOutputField(ctx context.Context, app core.App, record *core.Record, outputField string, content string) error {
	if record.GetString(outputField) == content {
		return nil
	}

	record.SetRaw(extractionInProgressKey, true)
	defer record.SetRaw(extractionInProgressKey, nil)

	record.Set(outputField, content)
	record.Set("updated", types.NowDateTime())

	return app.SaveNoValidateWithContext(ctx, record)
}

func pdfToText(inputPath string) (string, error) {
	f, err := os.Open(inputPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	pdfReader, err := pdf.NewPdfReader(f)
	if err != nil {
		return "", err
	}

	numPages, err := pdfReader.GetNumPages()
	if err != nil {
		return "", err
	}

	texts := make([]string, 0, numPages)
	for pageIndex := 1; pageIndex <= numPages; pageIndex++ {
		page, err := pdfReader.GetPage(pageIndex)
		if err != nil {
			return "", err
		}

		ex, err := extractor.New(page)
		if err != nil {
			return "", err
		}

		text, err := ex.ExtractText()
		if err != nil {
			return "", err
		}

		texts = append(texts, text)
	}

	return strings.Join(texts, "\n"), nil
}
