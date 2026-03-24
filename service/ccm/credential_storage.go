package ccm

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type jsonContainerStorage interface {
	readContainer() (map[string]json.RawMessage, bool, error)
	writeContainer(map[string]json.RawMessage) error
	delete() error
}

type jsonFileStorage struct {
	path string
}

func (s jsonFileStorage) readContainer() (map[string]json.RawMessage, bool, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]json.RawMessage), false, nil
		}
		return nil, false, err
	}
	container := make(map[string]json.RawMessage)
	if len(data) == 0 {
		return container, true, nil
	}
	if err := json.Unmarshal(data, &container); err != nil {
		return nil, true, err
	}
	return container, true, nil
}

func (s jsonFileStorage) writeContainer(container map[string]json.RawMessage) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(container, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o600)
}

func (s jsonFileStorage) delete() error {
	err := os.Remove(s.path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func writeStorageValue(storage jsonContainerStorage, key string, value any) error {
	container, _, err := storage.readContainer()
	if err != nil {
		var syntaxError *json.SyntaxError
		var typeError *json.UnmarshalTypeError
		if !errors.As(err, &syntaxError) && !errors.As(err, &typeError) {
			return err
		}
		container = make(map[string]json.RawMessage)
	}
	if container == nil {
		container = make(map[string]json.RawMessage)
	}
	encodedValue, err := json.Marshal(value)
	if err != nil {
		return err
	}
	container[key] = encodedValue
	return storage.writeContainer(container)
}

func persistStorageValue(primary jsonContainerStorage, fallback jsonContainerStorage, key string, value any) error {
	primaryErr := writeStorageValue(primary, key, value)
	if primaryErr == nil {
		if fallback != nil {
			_ = fallback.delete()
		}
		return nil
	}
	if fallback == nil {
		return primaryErr
	}
	if err := writeStorageValue(fallback, key, value); err != nil {
		return err
	}
	_ = primary.delete()
	return nil
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneBoolPointer(value *bool) *bool {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func equalStringPointer(left *string, right *string) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func equalBoolPointer(left *bool, right *bool) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}
