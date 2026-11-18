package api

import "github.com/getplinth/plinth/internal/store"

// nonNilStore returns a Store value that is never used for I/O: tests that use
// it only exercise paths that fail before touching the database.
func nonNilStore() *store.Store { return &store.Store{} }
