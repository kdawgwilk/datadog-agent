// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022-present Datadog, Inc.

package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMaxUint64(t *testing.T) {
	assert.Equal(t, uint64(10), MaxUint64(uint64(10), uint64(5)))
	assert.Equal(t, uint64(10), MaxUint64(uint64(5), uint64(10)))
}

func TestMinUint64(t *testing.T) {
	assert.Equal(t, uint64(5), MinUint64(uint64(10), uint64(5)))
	assert.Equal(t, uint64(5), MinUint64(uint64(5), uint64(10)))
}

func TestUint32ToBytes(t *testing.T) {
	const MinUint = uint32(0)
	const MaxUint = ^uint32(0)

	assert.Equal(t, Uint32ToBytes(MinUint), []byte{0, 0, 0, 0})
	assert.Equal(t, Uint32ToBytes(MaxUint), []byte{255, 255, 255, 255})
	assert.Equal(t, Uint32ToBytes(300), []byte{0x2c, 0x1, 0x0, 0x0})
}

func TestIPBytesToString(t *testing.T) {
	assert.Equal(t, "0.0.0.0", IPBytesToString([]byte{0, 0, 0, 0}))
	assert.Equal(t, "1.2.3.4", IPBytesToString([]byte{1, 2, 3, 4}))
	assert.Equal(t, "127.0.0.1", IPBytesToString([]byte{127, 0, 0, 1}))
	assert.Equal(t, "255.255.255.255", IPBytesToString([]byte{255, 255, 255, 255}))
}
