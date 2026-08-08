package domain

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"
)

type RunID string
type TurnID string
type OperationID string
type AttemptID string

func NewID(prefix string) string {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		panic(fmt.Errorf("read random id entropy: %w", err))
	}
	return fmt.Sprintf("%s_%d_%s", prefix, time.Now().UTC().UnixNano(), base64.RawURLEncoding.EncodeToString(entropy[:]))
}

func NewRunID() RunID             { return RunID(NewID("run")) }
func NewTurnID() TurnID           { return TurnID(NewID("turn")) }
func NewOperationID() OperationID { return OperationID(NewID("op")) }
func NewAttemptID() AttemptID     { return AttemptID(NewID("attempt")) }
