package projectionbatch

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"unicode/utf8"

	"github.com/zendext/scrape-fs/internal/namespace"
)

const (
	BatchSchema      = "scrapefs.projection-batch/v1"
	ValidationSchema = "scrapefs.projection-batch-validation/v1"
	ResultSchema     = "scrapefs.projection-batch-result/v1"
	ListSchema       = "scrapefs.projection-batch-list/v1"
	DetailSchema     = "scrapefs.projection-batch-detail/v1"
	UndoSchema       = "scrapefs.projection-batch-undo/v1"
	ErrorSchema      = "scrapefs.error/v1"
)

var ErrInvalidBatch = errors.New("invalid Projection Batch")

type Document struct {
	Schema  string  `json:"schema"`
	Library string  `json:"library"`
	Entries []Entry `json:"entries"`
}

type Entry struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

type Parsed struct {
	Document Document
	Digest   string
}

func Parse(data []byte) (Parsed, error) {
	if !utf8.Valid(data) {
		return Parsed{}, fmt.Errorf("%w: input must be valid UTF-8", ErrInvalidBatch)
	}
	if err := validateJSONStructure(data); err != nil {
		return Parsed{}, fmt.Errorf("%w: %v", ErrInvalidBatch, err)
	}

	var document Document
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return Parsed{}, fmt.Errorf("%w: decode JSON: %v", ErrInvalidBatch, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Parsed{}, fmt.Errorf("%w: %v", ErrInvalidBatch, err)
	}
	if err := validateDocument(document); err != nil {
		return Parsed{}, err
	}

	canonical, err := json.Marshal(document)
	if err != nil {
		return Parsed{}, fmt.Errorf("encode canonical Projection Batch: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return Parsed{
		Document: document,
		Digest:   "sha256:" + hex.EncodeToString(sum[:]),
	}, nil
}

func validateDocument(document Document) error {
	if document.Schema != BatchSchema {
		return fmt.Errorf(
			"%w: schema must be %q",
			ErrInvalidBatch,
			BatchSchema,
		)
	}
	libraryComponents, err := namespace.ValidateRelativePath(document.Library)
	if err != nil || len(libraryComponents) != 1 {
		return fmt.Errorf(
			"%w: invalid Library name %q",
			ErrInvalidBatch,
			document.Library,
		)
	}
	if len(document.Entries) == 0 {
		return fmt.Errorf("%w: entries must not be empty", ErrInvalidBatch)
	}

	sources := make(map[string]struct{}, len(document.Entries))
	targets := make(map[string][]string, len(document.Entries))
	for index, entry := range document.Entries {
		if !filepath.IsAbs(entry.Source) || filepath.Clean(entry.Source) != entry.Source {
			return fmt.Errorf(
				"%w: entry %d source must be a normalized absolute path",
				ErrInvalidBatch,
				index,
			)
		}
		if _, duplicate := sources[entry.Source]; duplicate {
			return fmt.Errorf(
				"%w: duplicate source %q",
				ErrInvalidBatch,
				entry.Source,
			)
		}
		sources[entry.Source] = struct{}{}

		components, err := namespace.ValidateRelativePath(entry.Target)
		if err != nil {
			return fmt.Errorf(
				"%w: entry %d target: %v",
				ErrInvalidBatch,
				index,
				err,
			)
		}
		if _, duplicate := targets[entry.Target]; duplicate {
			return fmt.Errorf(
				"%w: duplicate target %q",
				ErrInvalidBatch,
				entry.Target,
			)
		}
		targets[entry.Target] = components
	}

	for target, components := range targets {
		for otherTarget, otherComponents := range targets {
			if target == otherTarget {
				continue
			}
			if isComponentPrefix(components, otherComponents) {
				return fmt.Errorf(
					"%w: target %q would be a parent of target %q",
					ErrInvalidBatch,
					target,
					otherTarget,
				)
			}
		}
	}
	return nil
}

func isComponentPrefix(prefix, value []string) bool {
	if len(prefix) >= len(value) {
		return false
	}
	for index := range prefix {
		if prefix[index] != value[index] {
			return false
		}
	}
	return true
}

func validateJSONStructure(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	return requireJSONEOF(decoder)
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}

	switch delimiter {
	case '{':
		fields := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if _, duplicate := fields[key]; duplicate {
				return fmt.Errorf("duplicate object field %q", key)
			}
			fields[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closeToken, err := decoder.Token()
		if err != nil {
			return err
		}
		if closeToken != json.Delim('}') {
			return fmt.Errorf("object is not closed")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closeToken, err := decoder.Token()
		if err != nil {
			return err
		}
		if closeToken != json.Delim(']') {
			return fmt.Errorf("array is not closed")
		}
	default:
		return fmt.Errorf("unexpected delimiter %q", delimiter)
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("input contains more than one JSON value")
		}
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}
