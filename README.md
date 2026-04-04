# PDF Text Extractor

Extract text from PDF files in PocketBase file fields and store the result in another field.

The plugin supports multi-file fields, ignores non-PDF uploads, and merges multiple extracted PDFs with `---` separators.

## Installation

Build PocketBase with the plugin:

```sh
xpb build --with github.com/mjadobson/pb-plugin-pdf@latest
```

## Setup

During app initialization, the plugin creates a shared `_plugins` collection if it doesn't already exist.
If the plugin is loaded before PocketBase has finished bootstrapping, this setup is deferred until the bootstrap step completes.

The collection includes these fields:

- `plugin_name` (`text`)
- `config` (`json`)
- `enabled` (`bool`)

To configure this plugin, add a row to `_plugins` with:

- `plugin_name` = `pdf_text_extractor`
- `enabled` = `true`
- `config` containing a JSON array of extraction rules

Example:

```json
[
  {
    "collection_name": "docs",
    "input_field": "files",
    "output_field": "files_text",
    "recalculate": true
  },
  {
    "collection_name": "invoices",
    "input_field": "pdf",
    "output_field": "pdf_text"
  }
]
```

For each configured rule, make sure the target collection has:

- a file field matching `input_field`
- a text or editor field matching `output_field`

The plugin runs after successful record creation, and after updates only when the configured file field value changes.

## Plugin Config

### `config[].collection_name`

The PocketBase collection name to watch.

### `config[].input_field`

The file field containing one or more uploads.

### `config[].output_field`

The text or editor field where extracted content should be stored.

### `config[].recalculate`

Optional one-off trigger. When set to `true`, the plugin will backfill all rows in the configured collection in batches of 100.

As soon as the job starts, the plugin removes `recalculate` and sets `recalculating: true` for that config entry. When the job finishes, it removes `recalculating` too.

### `config[].recalculating`

Transient status flag managed by the plugin while a one-off recalculation is running. You should not set this manually.

## Behaviour

- Empty input clears the output field.
- Only `.pdf` files are processed.
- Non-PDF files are ignored.
- Multiple PDFs are joined with `---` on its own line.
- Extraction failures are logged and skipped so other files can still be processed.
- Unrelated record updates do not re-parse PDFs; text is refreshed only when the configured file field changes.
- Changing `_plugins` rows or relevant collection schemas takes effect for future create/update events without backfilling existing records.
- Setting `recalculate: true` on a config entry triggers a one-off backfill for existing rows in that collection.

## Development

```sh
go mod tidy
GOCACHE=/tmp/go-build go test ./...
GOCACHE=/tmp/go-build go build ./...
```

## License

Licensed under the GNU Affero General Public License v3.0.

This plugin links against `github.com/moolekkari/unipdf`, which is AGPL-licensed, so the plugin is distributed under AGPLv3 as well. See [LICENSE](./LICENSE).
