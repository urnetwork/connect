//go:build !acklineagetrace

package connect

const ackLineageTraceEnabled = false

func observeAckLineagePacing(*SendSequence, *sendItem, bool) {}
