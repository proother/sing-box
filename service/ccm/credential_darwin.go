//go:build darwin && cgo

package ccm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	E "github.com/sagernet/sing/common/exceptions"

	"github.com/keybase/go-keychain"
)

type keychainStorage struct {
	service string
	account string
}

func getKeychainServiceName() string {
	configDirectory := os.Getenv("CLAUDE_CONFIG_DIR")
	if configDirectory == "" {
		return "Claude Code-credentials"
	}

	userInfo, err := getRealUser()
	if err != nil {
		return "Claude Code-credentials"
	}
	defaultConfigDirectory := filepath.Join(userInfo.HomeDir, ".claude")
	if configDirectory == defaultConfigDirectory {
		return "Claude Code-credentials"
	}

	hash := sha256.Sum256([]byte(configDirectory))
	return "Claude Code-credentials-" + hex.EncodeToString(hash[:])[:8]
}

func platformReadCredentials(customPath string) (*oauthCredentials, error) {
	if customPath != "" {
		return readCredentialsFromFile(customPath)
	}

	userInfo, err := getRealUser()
	if err == nil {
		query := keychain.NewItem()
		query.SetSecClass(keychain.SecClassGenericPassword)
		query.SetService(getKeychainServiceName())
		query.SetAccount(userInfo.Username)
		query.SetMatchLimit(keychain.MatchLimitOne)
		query.SetReturnData(true)

		results, err := keychain.QueryItem(query)
		if err == nil && len(results) == 1 {
			var container struct {
				ClaudeAIAuth *oauthCredentials `json:"claudeAiOauth,omitempty"`
			}
			unmarshalErr := json.Unmarshal(results[0].Data, &container)
			if unmarshalErr == nil && container.ClaudeAIAuth != nil {
				return container.ClaudeAIAuth, nil
			}
		}
		if err != nil && err != keychain.ErrorItemNotFound {
			return nil, E.Cause(err, "query keychain")
		}
	}

	defaultPath, err := getDefaultCredentialsPath()
	if err != nil {
		return nil, err
	}
	return readCredentialsFromFile(defaultPath)
}

func platformCanWriteCredentials(customPath string) error {
	if customPath == "" {
		return nil
	}
	return checkCredentialFileWritable(customPath)
}

func platformWriteCredentials(credentials *oauthCredentials, customPath string) error {
	if customPath != "" {
		return writeCredentialsToFile(credentials, customPath)
	}

	defaultPath, err := getDefaultCredentialsPath()
	if err != nil {
		return err
	}
	fileStorage := jsonFileStorage{path: defaultPath}

	userInfo, err := getRealUser()
	if err != nil {
		return writeCredentialsToFile(credentials, defaultPath)
	}
	return persistStorageValue(keychainStorage{
		service: getKeychainServiceName(),
		account: userInfo.Username,
	}, fileStorage, "claudeAiOauth", credentials)
}

func (s keychainStorage) readContainer() (map[string]json.RawMessage, bool, error) {
	query := keychain.NewItem()
	query.SetSecClass(keychain.SecClassGenericPassword)
	query.SetService(s.service)
	query.SetAccount(s.account)
	query.SetMatchLimit(keychain.MatchLimitOne)
	query.SetReturnData(true)

	results, err := keychain.QueryItem(query)
	if err != nil {
		if err == keychain.ErrorItemNotFound {
			return make(map[string]json.RawMessage), false, nil
		}
		return nil, false, E.Cause(err, "query keychain")
	}
	if len(results) != 1 {
		return make(map[string]json.RawMessage), false, nil
	}

	container := make(map[string]json.RawMessage)
	if len(results[0].Data) == 0 {
		return container, true, nil
	}
	if err := json.Unmarshal(results[0].Data, &container); err != nil {
		return nil, true, err
	}
	return container, true, nil
}

func (s keychainStorage) writeContainer(container map[string]json.RawMessage) error {
	data, err := json.Marshal(container)
	if err != nil {
		return err
	}

	item := keychain.NewItem()
	item.SetSecClass(keychain.SecClassGenericPassword)
	item.SetService(s.service)
	item.SetAccount(s.account)
	item.SetData(data)
	item.SetAccessible(keychain.AccessibleWhenUnlocked)
	err = keychain.AddItem(item)
	if err == nil {
		return nil
	}
	if err != keychain.ErrorDuplicateItem {
		return err
	}

	updateQuery := keychain.NewItem()
	updateQuery.SetSecClass(keychain.SecClassGenericPassword)
	updateQuery.SetService(s.service)
	updateQuery.SetAccount(s.account)

	updateItem := keychain.NewItem()
	updateItem.SetData(data)
	return keychain.UpdateItem(updateQuery, updateItem)
}

func (s keychainStorage) delete() error {
	err := keychain.DeleteGenericPasswordItem(s.service, s.account)
	if err != nil && err != keychain.ErrorItemNotFound {
		return err
	}
	return nil
}
