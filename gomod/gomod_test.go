package gomod

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAll(t *testing.T) {
	assert.NotEmpty(t, All())
}

func TestGet(t *testing.T) {
	assert.NotEmpty(t, Get("testify").Path())
}

func TestRoot(t *testing.T) {
	m := Root(assert.Equal)
	assert.Equal(t, "testify", m.Name())
	assert.Regexp(t, `^\d+\.\d+\.\d+`, m.Version())
}
