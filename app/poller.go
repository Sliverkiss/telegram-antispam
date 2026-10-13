package main

// The long-polling loop is driven by github.com/go-telegram/bot's Start(ctx)
// (configured via telegram.Client and dispatched in runPoller in main.go).
// This file is kept as the poller's documentation anchor; the old hand-written
// getUpdates loop was removed with the library swap.
