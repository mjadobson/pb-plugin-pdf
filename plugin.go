package pdf_text_extractor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

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

type ExtractPdfTextConfig struct {
	CollectionName string `json:"collection_name"`
	InputField     string `json:"input_field"`
	OutputField    string `json:"output_field"`
}

type Plugin struct {
	Configs []ExtractPdfTextConfig `json:"configs"`
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
	if err := p.validateConfigShapes(); err != nil {
		return err
	}

	for _, cfg := range p.Configs {
		config := cfg

		app.OnRecordAfterCreateSuccess(config.CollectionName).BindFunc(func(e *core.RecordEvent) error {
			if err := processRecord(e.Context, e.App, config, e.Record); err != nil {
				e.App.Logger().Error(
					"pdf_text_extractor create hook failed",
					slog.String("collection", config.CollectionName),
					slog.String("recordId", e.Record.Id),
					slog.Any("error", err),
				)
			}

			return e.Next()
		})

		app.OnRecordAfterUpdateSuccess(config.CollectionName).BindFunc(func(e *core.RecordEvent) error {
			if err := processRecord(e.Context, e.App, config, e.Record); err != nil {
				e.App.Logger().Error(
					"pdf_text_extractor update hook failed",
					slog.String("collection", config.CollectionName),
					slog.String("recordId", e.Record.Id),
					slog.Any("error", err),
				)
			}

			return e.Next()
		})
	}

	return nil
}

func (p *Plugin) validateConfigShapes() error {
	if len(p.Configs) == 0 {
		return fmt.Errorf("%s: at least one config entry is required", p.Name())
	}

	for i, cfg := range p.Configs {
		if cfg.CollectionName == "" || cfg.InputField == "" || cfg.OutputField == "" {
			return fmt.Errorf("%s: config %d must include collection_name, input_field, and output_field", p.Name(), i)
		}
	}

	return nil
}

func (p *Plugin) validateStartupConfigs(app core.App) error {
	for i, cfg := range p.Configs {
		collection, err := app.FindCachedCollectionByNameOrId(cfg.CollectionName)
		if err != nil {
			app.Logger().Warn(
				"pdf_text_extractor config warning",
				slog.Int("configIndex", i),
				slog.String("collection", cfg.CollectionName),
				slog.Any("error", fmt.Errorf("collection %q not found yet: %w", cfg.CollectionName, err)),
			)
			continue
		}

		if err := validateConfigForCollection(cfg, collection); err != nil {
			return fmt.Errorf("%s: config %d invalid: %w", p.Name(), i, err)
		}
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

	if err := app.SaveNoValidateWithContext(ctx, record); err != nil {
		// Best-effort fallback to keep extraction functional even if a nested save is blocked.
		_, dbErr := app.DB().
			Update(
				record.TableName(),
				dbx.Params{
					outputField: content,
					"updated":   record.GetDateTime("updated"),
				},
				dbx.HashExp{"id": record.Id},
			).
			Execute()
		if dbErr != nil {
			return errors.Join(err, dbErr)
		}
	}

	return nil
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
