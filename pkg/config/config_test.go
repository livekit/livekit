package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConfig_UnmarshalKeys(t *testing.T) {
	conf, err := NewConfig("")
	assert.NoError(t, err)

	assert.NoError(t, conf.unmarshalKeys("key1: secret1"))
	assert.Equal(t, "secret1", conf.Keys["key1"])
}
