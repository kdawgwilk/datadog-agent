// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build linux_bpf
// +build linux_bpf

package netlink

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/DataDog/datadog-agent/pkg/ebpf"
	"github.com/DataDog/datadog-agent/pkg/network/config"
	nettestutil "github.com/DataDog/datadog-agent/pkg/network/testutil"
)

func TestConsumerKeepsRunningAfterCircuitBreakerTrip(t *testing.T) {
	c, err := NewConsumer(
		&config.Config{
			Config: ebpf.Config{
				ProcRoot: "/proc",
			},
			ConntrackRateLimit:           1,
			EnableRootNetNs:              true,
			EnableConntrackAllNamespaces: false,
		})
	require.NoError(t, err)
	require.NotNil(t, c)
	exited := make(chan struct{})
	defer func() {
		c.Stop()
		<-exited
	}()

	ev, err := c.Events()
	require.NoError(t, err)
	require.NotNil(t, ev)

	go func() {
		for range ev {
		}

		close(exited)
	}()

	isRecvLoopRunning := func() bool {
		return c.recvLoopRunning.Load()
	}

	require.Eventually(t, func() bool {
		return isRecvLoopRunning()
	}, 3*time.Second, 100*time.Millisecond)

	srv := nettestutil.StartServerTCP(t, net.ParseIP("127.0.0.1"), 0)
	defer srv.Close()

	l := srv.(net.Listener)

	// this should trip the circuit breaker
	// we have to run this loop for more than
	// `tickInterval` seconds (3s currently) for
	// the circuit breaker to detect the over-limit
	// rate of updates
	for i := 0; i < 16; i++ {
		conn, err := net.Dial("tcp", l.Addr().String())
		require.NoError(t, err)
		defer conn.Close()

		time.Sleep(250 * time.Millisecond)
	}

	// on pre 3.15 kernels, the receive loop
	// will simply bail since bpf random sampling
	// is not available
	if pre315Kernel {
		require.Eventually(t, func() bool {
			return !isRecvLoopRunning() && c.breaker.IsOpen()
		}, 3*time.Second, 100*time.Millisecond)
	} else {
		require.Eventually(t, func() bool {
			return c.samplingRate < 1.0
		}, 3*time.Second, 100*time.Millisecond)

		require.True(t, isRecvLoopRunning())
	}

}
