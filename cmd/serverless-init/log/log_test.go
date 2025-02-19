// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package log

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/DataDog/datadog-agent/cmd/serverless-init/metadata"
	"github.com/DataDog/datadog-agent/pkg/logs/config"
	"github.com/stretchr/testify/assert"
)

func TestCustomWriterBuffered(t *testing.T) {
	testContent := []byte("log line\nlog line\n")
	config := &Config{
		channel:   make(chan *config.ChannelMessage, 2),
		isEnabled: true,
	}
	cw := &CustomWriter{
		LogConfig:  config,
		LineBuffer: bytes.Buffer{},
	}
	go cw.Write(testContent)
	numMessages := 0
	select {
	case message := <-config.channel:
		assert.Equal(t, []byte("log line"), message.Content)
		numMessages++
	case <-time.After(100 * time.Millisecond):
		t.FailNow()
	}

	select {
	case message := <-config.channel:
		assert.Equal(t, []byte("log line"), message.Content)
		numMessages++
	case <-time.After(100 * time.Millisecond):
		t.FailNow()
	}

	assert.Equal(t, 2, numMessages)
}

func TestWriteEnabled(t *testing.T) {
	testContent := []byte("hello this is a log")
	logChannel := make(chan *config.ChannelMessage)
	config := &Config{
		channel:   logChannel,
		isEnabled: true,
	}
	go Write(config, testContent, false)
	select {
	case received := <-logChannel:
		assert.NotNil(t, received)
		assert.Equal(t, testContent, received.Content)
	case <-time.After(100 * time.Millisecond):
		assert.Fail(t, "We should have received logs")
	}
}

func TestWriteEnabledIsError(t *testing.T) {
	testContent := []byte("hello this is a log")
	logChannel := make(chan *config.ChannelMessage)
	config := &Config{
		channel:   logChannel,
		isEnabled: true,
	}
	go Write(config, testContent, true)
	select {
	case received := <-logChannel:
		assert.NotNil(t, received)
		assert.Equal(t, testContent, received.Content)
		assert.True(t, received.IsError)
	case <-time.After(100 * time.Millisecond):
		assert.Fail(t, "We should have received logs")
	}
}

func TestWriteDisabled(t *testing.T) {
	testContent := []byte("hello this is a log")
	logChannel := make(chan *config.ChannelMessage)
	config := &Config{
		channel:   logChannel,
		isEnabled: false,
	}
	go Write(config, testContent, false)
	select {
	case <-logChannel:
		assert.Fail(t, "We should not have received logs")
	case <-time.After(100 * time.Millisecond):
		assert.True(t, true)
	}
}

func TestCreateConfig(t *testing.T) {
	metadata := &metadata.Metadata{}
	config := CreateConfig(metadata)
	assert.Equal(t, 5*time.Second, config.FlushTimeout)
	assert.Equal(t, "cloudrun", config.source)
	assert.Equal(t, "DD_CLOUDRUN_LOG_AGENT", string(config.loggerName))
	assert.Equal(t, metadata, config.Metadata)
}

func TestCreateConfigWithSource(t *testing.T) {
	os.Setenv("DD_SOURCE", "python")
	defer os.Unsetenv("DD_SOURCE")
	metadata := &metadata.Metadata{}
	config := CreateConfig(metadata)
	assert.Equal(t, 5*time.Second, config.FlushTimeout)
	assert.Equal(t, "python", config.source)
	assert.Equal(t, "DD_CLOUDRUN_LOG_AGENT", string(config.loggerName))
	assert.Equal(t, metadata, config.Metadata)
}

func TestIsEnabledTrue(t *testing.T) {
	assert.True(t, isEnabled("True"))
	assert.True(t, isEnabled("TRUE"))
	assert.True(t, isEnabled("true"))
}

func TestIsEnabledFalse(t *testing.T) {
	assert.False(t, isEnabled(""))
	assert.False(t, isEnabled("false"))
	assert.False(t, isEnabled("1"))
	assert.False(t, isEnabled("FALSE"))
}
