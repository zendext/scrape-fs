package namespace

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	maxComponentBytes    = 255
	maxRelativePathBytes = 4095
)

func ValidateRelativePath(value string) ([]string, error) {
	if value == "" {
		return nil, fmt.Errorf("%w: path must not be empty", ErrInvalidPath)
	}
	if !utf8.ValidString(value) {
		return nil, fmt.Errorf("%w: path must be valid UTF-8", ErrInvalidPath)
	}
	if len(value) > maxRelativePathBytes {
		return nil, fmt.Errorf(
			"%w: path exceeds %d bytes",
			ErrInvalidPath,
			maxRelativePathBytes,
		)
	}
	if strings.HasPrefix(value, "/") {
		return nil, fmt.Errorf("%w: path must be relative", ErrInvalidPath)
	}

	components := strings.Split(value, "/")
	if err := validateComponents(components); err != nil {
		return nil, err
	}
	return components, nil
}

func validateComponents(components []string) error {
	totalBytes := 0
	for index, component := range components {
		if component == "" {
			return fmt.Errorf("%w: path contains an empty component", ErrInvalidPath)
		}
		if component == "." || component == ".." {
			return fmt.Errorf(
				"%w: component %q is reserved",
				ErrInvalidPath,
				component,
			)
		}
		if !utf8.ValidString(component) {
			return fmt.Errorf("%w: path must be valid UTF-8", ErrInvalidPath)
		}
		if strings.IndexByte(component, 0) >= 0 {
			return fmt.Errorf("%w: path contains NUL", ErrInvalidPath)
		}
		if strings.Contains(component, "/") {
			return fmt.Errorf("%w: component contains a separator", ErrInvalidPath)
		}
		if len(component) > maxComponentBytes {
			return fmt.Errorf(
				"%w: component %q exceeds %d bytes",
				ErrInvalidPath,
				component,
				maxComponentBytes,
			)
		}
		totalBytes += len(component)
		if index > 0 {
			totalBytes++
		}
	}
	if totalBytes > maxRelativePathBytes {
		return fmt.Errorf(
			"%w: path exceeds %d bytes",
			ErrInvalidPath,
			maxRelativePathBytes,
		)
	}
	return nil
}

func validateName(name string) error {
	return validateComponents([]string{name})
}
