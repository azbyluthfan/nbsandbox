// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package logs captures run output: one reader per run, sequence-numbered
// lines in log.txt, fan-out to live viewers with replay and resume, and the
// per-run log cap.
package logs
