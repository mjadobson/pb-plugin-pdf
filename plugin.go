package pdf_text_extractor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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
const inputChangedKeyPrefix = "@pdf_text_extractor_input_changed:"
const pluginsCollectionName = "_plugins"
const pluginNameField = "plugin_name"
const configField = "config"
const enabledField = "enabled"
const recalculateBatchSize = 100

type ExtractPdfTextConfig struct {
	CollectionName string `json:"collection_name"`
	InputField     string `json:"input_field"`
	OutputField    string `json:"output_field"`
	Recalculate    bool   `json:"recalculate,omitempty"`
	Recalculating  bool   `json:"recalculating,omitempty"`
}

type Plugin struct {
	state *pluginState
}

type pluginState struct {
	pluginName           string
	mu                   sync.RWMutex
	byColl               map[string][]ExtractPdfTextConfig
	activeRecalculations map[string]struct{}
}

func newPluginState(pluginName string) *pluginState {
	return &pluginState{
		pluginName:           pluginName,
		byColl:               map[string][]ExtractPdfTextConfig{},
		activeRecalculations: map[string]struct{}{},
	}
}

type pendingRecalculation struct {
	rowID       string
	configIndex int
	config      ExtractPdfTextConfig
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

	if app.IsBootstrapped() {
		if err := p.refreshState(app); err != nil {
			return err
		}
	} else {
		app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}

			return p.refreshState(e.App)
		})
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

	app.OnRecordUpdate().BindFunc(func(e *core.RecordEvent) error {
		p.markChangedInputFields(e)
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
		if e.Type == "update" && !didInputFieldChange(e.Record, cfg.InputField) {
			continue
		}

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

func (p *Plugin) markChangedInputFields(e *core.RecordEvent) {
	if p.state == nil || e.Record.Collection().Name == pluginsCollectionName {
		return
	}

	configs := p.state.configsForCollection(e.Record.Collection().Name)
	if len(configs) == 0 {
		return
	}

	oldRecord, err := e.App.FindRecordById(e.Record.Collection(), e.Record.Id)
	if err != nil {
		for _, cfg := range configs {
			e.Record.SetRaw(inputChangedKeyPrefix+cfg.InputField, true)
		}
		return
	}

	for _, cfg := range configs {
		e.Record.SetRaw(inputChangedKeyPrefix+cfg.InputField, didInputFieldChangeBeforeSave(oldRecord, e.Record, cfg.InputField))
	}
}

func didInputFieldChange(record *core.Record, fieldName string) bool {
	changed, _ := record.GetRaw(inputChangedKeyPrefix + fieldName).(bool)
	return changed
}

func didInputFieldChangeBeforeSave(oldRecord *core.Record, record *core.Record, fieldName string) bool {
	if len(record.GetUnsavedFiles(fieldName)) > 0 {
		return true
	}

	return !slices.Equal(oldRecord.GetStringSlice(fieldName), record.GetStringSlice(fieldName))
}

func (p *Plugin) refreshState(app core.App) error {
	if err := ensurePluginsCollection(app); err != nil {
		return err
	}

	pending, err := p.state.reload(app)
	if err != nil {
		return err
	}

	for _, job := range pending {
		go p.runRecalculation(app, job)
	}

	return nil
}

func (s *pluginState) reload(app core.App) ([]pendingRecalculation, error) {
	rows, err := app.FindAllRecords(
		pluginsCollectionName,
		dbx.HashExp{
			pluginNameField: s.pluginName,
			enabledField:    true,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("load plugin configs: %w", err)
	}

	next := make(map[string][]ExtractPdfTextConfig)
	pending := make([]pendingRecalculation, 0)

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

			if cfg.Recalculate {
				if s.tryStartRecalculation(row.Id, i) {
					pending = append(pending, pendingRecalculation{
						rowID:       row.Id,
						configIndex: i,
						config:      cfg,
					})
				}
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

	return pending, nil
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

func (s *pluginState) tryStartRecalculation(rowID string, configIndex int) bool {
	key := recalculationKey(rowID, configIndex)

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.activeRecalculations[key]; ok {
		return false
	}

	s.activeRecalculations[key] = struct{}{}
	return true
}

func (s *pluginState) finishRecalculation(rowID string, configIndex int) {
	s.mu.Lock()
	delete(s.activeRecalculations, recalculationKey(rowID, configIndex))
	s.mu.Unlock()
}

func recalculationKey(rowID string, configIndex int) string {
	return fmt.Sprintf("%s:%d", rowID, configIndex)
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

func (p *Plugin) runRecalculation(app core.App, job pendingRecalculation) {
	defer p.state.finishRecalculation(job.rowID, job.configIndex)

	row, cfg, err := p.markConfigRecalculating(context.Background(), app, job)
	if err != nil {
		app.Logger().Error(
			"pdf_text_extractor failed to start recalculation",
			slog.String("configRecordId", job.rowID),
			slog.Int("configIndex", job.configIndex),
			slog.Any("error", err),
		)
		return
	}

	if err := p.recalculateCollection(context.Background(), app, cfg); err != nil {
		app.Logger().Error(
			"pdf_text_extractor recalculation failed",
			slog.String("configRecordId", job.rowID),
			slog.Int("configIndex", job.configIndex),
			slog.String("collection", cfg.CollectionName),
			slog.String("inputField", cfg.InputField),
			slog.String("outputField", cfg.OutputField),
			slog.Any("error", err),
		)
	}

	if err := p.clearConfigRecalculating(context.Background(), app, row, job.configIndex); err != nil {
		app.Logger().Error(
			"pdf_text_extractor failed to finish recalculation",
			slog.String("configRecordId", job.rowID),
			slog.Int("configIndex", job.configIndex),
			slog.Any("error", err),
		)
	}
}

func (p *Plugin) markConfigRecalculating(ctx context.Context, app core.App, job pendingRecalculation) (*core.Record, ExtractPdfTextConfig, error) {
	row, err := app.FindRecordById(pluginsCollectionName, job.rowID)
	if err != nil {
		return nil, ExtractPdfTextConfig{}, err
	}

	configs, err := parsePluginConfigs(row)
	if err != nil {
		return nil, ExtractPdfTextConfig{}, err
	}

	if job.configIndex >= len(configs) {
		return nil, ExtractPdfTextConfig{}, fmt.Errorf("config index %d out of range", job.configIndex)
	}

	cfg := configs[job.configIndex]
	if !cfg.Recalculate && !cfg.Recalculating {
		return row, cfg, nil
	}

	cfg.Recalculate = false
	cfg.Recalculating = true
	configs[job.configIndex] = cfg

	if err := savePluginConfigs(ctx, app, row, configs); err != nil {
		return nil, ExtractPdfTextConfig{}, err
	}

	return row, cfg, nil
}

func (p *Plugin) clearConfigRecalculating(ctx context.Context, app core.App, row *core.Record, configIndex int) error {
	refreshed, err := app.FindRecordById(pluginsCollectionName, row.Id)
	if err != nil {
		return err
	}

	configs, err := parsePluginConfigs(refreshed)
	if err != nil {
		return err
	}

	if configIndex >= len(configs) {
		return fmt.Errorf("config index %d out of range", configIndex)
	}

	configs[configIndex].Recalculating = false

	return savePluginConfigs(ctx, app, refreshed, configs)
}

func savePluginConfigs(ctx context.Context, app core.App, row *core.Record, configs []ExtractPdfTextConfig) error {
	raw, err := json.Marshal(configs)
	if err != nil {
		return fmt.Errorf("encode config json: %w", err)
	}

	row.Set(configField, types.JSONRaw(raw))
	return app.SaveNoValidateWithContext(ctx, row)
}

func (p *Plugin) recalculateCollection(ctx context.Context, app core.App, cfg ExtractPdfTextConfig) error {
	collection, err := app.FindCollectionByNameOrId(cfg.CollectionName)
	if err != nil {
		return err
	}

	offset := 0
	for {
		records, err := app.FindRecordsByFilter(collection, "", "id", recalculateBatchSize, offset)
		if err != nil {
			return err
		}

		if len(records) == 0 {
			return nil
		}

		for _, record := range records {
			if err := processRecord(ctx, app, cfg, record); err != nil {
				app.Logger().Error(
					"pdf_text_extractor batch recalculation failed for record",
					slog.String("collection", cfg.CollectionName),
					slog.String("recordId", record.Id),
					slog.String("inputField", cfg.InputField),
					slog.String("outputField", cfg.OutputField),
					slog.Any("error", err),
				)
			}
		}

		if len(records) < recalculateBatchSize {
			return nil
		}

		offset += len(records)
	}
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
