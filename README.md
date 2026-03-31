# PDF Text Extractor

Extract text from PDF files in PocketBase file fields and store the result in another field.

The plugin supports multi-file fields, ignores non-PDF uploads, and merges multiple extracted PDFs with `---` separators.

## Installation

Build PocketBase with the plugin:

```sh
xpb build --with github.com/matthewdobson/pb-plugin-pdf@latest
```

## Setup

Add the plugin config to your `pocketbuilds.toml`:

```toml
[pdf_text_extractor]

[[pdf_text_extractor.configs]]
collection_name = "docs"
input_field = "files"
output_field = "files_text"

[[pdf_text_extractor.configs]]
collection_name = "invoices"
input_field = "pdf"
output_field = "pdf_text"
```

Then make sure your collection has:

- a file field matching `input_field`
- a text field matching `output_field`

The plugin runs after successful record create and update operations.

## Plugin Config

### `configs`

An array of extraction rules.

### `configs[].collection_name`

The PocketBase collection name to watch.

### `configs[].input_field`

The file field containing one or more uploads.

### `configs[].output_field`

The text field where extracted content should be stored.

## Behaviour

- Empty input clears the output field.
- Only `.pdf` files are processed.
- Non-PDF files are ignored.
- Multiple PDFs are joined with `---` on its own line.
- Extraction failures are logged and skipped so other files can still be processed.

## Development

```sh
go mod tidy
GOCACHE=/tmp/go-build go test ./...
GOCACHE=/tmp/go-build go build ./...
```

## License

Licensed under the GNU Affero General Public License v3.0.

This plugin links against `github.com/moolekkari/unipdf`, which is AGPL-licensed, so the plugin is distributed under AGPLv3 as well. See [LICENSE](./LICENSE).
