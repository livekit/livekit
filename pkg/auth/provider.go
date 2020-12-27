package auth

import (
	"io"

	"gopkg.in/yaml.v3"
)

type FileBasedKeyProvider struct {
	keys map[string]string
}

func NewFileBasedKeyProvider(r io.Reader) (p *FileBasedKeyProvider, err error) {
	keys := make(map[string]string)
	decoder := yaml.NewDecoder(r)
	if err = decoder.Decode(&keys); err != nil {
		return
	}
	p = &FileBasedKeyProvider{
		keys: keys,
	}

	return
}

func (p *FileBasedKeyProvider) GetSecret(key string) string {
	return p.keys[key]
}

func (p *FileBasedKeyProvider) NumKeys() int {
	return len(p.keys)
}
