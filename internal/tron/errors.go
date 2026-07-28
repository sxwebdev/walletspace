package tron

import "errors"

// ErrInvalidRequest marks failures caused by the caller's input rather than by
// the network, so the HTTP layer can answer 400 instead of 502.
//
// Amount validation is not part of it: the gotron amount constructors already
// reject anything unrepresentable with client.ErrInvalidAmount, which the HTTP
// layer treats the same way.
var ErrInvalidRequest = errors.New("invalid request")
