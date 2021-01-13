package test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestScenarioDefault(t *testing.T) {
	assert.True(t, true)
}

func TestMain(m *testing.M) {
	s := createServer()
	go func() {
		s.Start()
	}()

	waitForServerToStart(s)

	code := m.Run()

	s.Stop()
	os.Exit(code)
}
