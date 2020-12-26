package auth

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"strings"
)

type FileBasedKeyProvider struct {
	keys map[string]string
}

func NewFileBasedKeyProvider(r io.Reader) (p *FileBasedKeyProvider, err error) {
	scanner := bufio.NewScanner(r)
	keys := make(map[string]string)
	for scanner.Scan() {
		line := scanner.Text()
		log.Println("line", line)
		//break
		if len(line) == 0 {
			continue
		}
		parts := strings.Split(string(line), ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid api key/secret pair, must be api_key:secret: %v", line)
		}
		keys[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}

	if err = scanner.Err(); err != nil {
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
