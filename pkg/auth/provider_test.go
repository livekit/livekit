package auth_test

import (
	"io/ioutil"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/livekit/livekit-server/pkg/auth"
)

func TestFileBasedKeyProvider(t *testing.T) {
	keys := map[string]string{
		"key1": "secret1",
		"key2": "secret2",
		"key3": "secret3",
	}
	f, err := ioutil.TempFile("", "keyfile")
	assert.NoError(t, err)
	defer func() {
		os.Remove(f.Name())
	}()

	f.WriteString("key1: secret1\n")
	f.WriteString("key2: secret2 \r\n")
	f.WriteString("\n")
	f.WriteString("key3: secret3")

	f.Close()

	r, err := os.Open(f.Name())
	defer r.Close()
	p, err := auth.NewFileBasedKeyProvider(r)
	assert.NoError(t, err)

	for key, val := range keys {
		assert.Equal(t, val, p.GetSecret(key))
	}
}
