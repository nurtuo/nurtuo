// SPDX-License-Identifier: AGPL-3.0-only

package docs

import "errors"

// Set is an ordered collection of documents processed together.
type Set struct {
	docs []*Document
}

// NewSet returns a set holding docs (slice is not copied).
func NewSet(docs ...*Document) *Set { return &Set{docs: docs} }

// Add appends a document.
func (s *Set) Add(d *Document) { s.docs = append(s.docs, d) }

// Len returns the number of documents.
func (s *Set) Len() int { return len(s.docs) }

// At returns the i-th document.
func (s *Set) At(i int) *Document { return s.docs[i] }

// Docs returns the underlying slice.
func (s *Set) Docs() []*Document { return s.docs }

// Counts is a per-status tally.
type Counts struct{ OK, Error, Dropped int }

// Counts tallies document statuses.
func (s *Set) Counts() Counts {
	var c Counts
	for _, d := range s.docs {
		switch d.status {
		case StatusOK:
			c.OK++
		case StatusError:
			c.Error++
		case StatusDropped:
			c.Dropped++
		}
	}
	return c
}

// Filter returns a new set of documents with the given status, sharing the documents.
func (s *Set) Filter(st Status) *Set {
	out := &Set{}
	for _, d := range s.docs {
		if d.status == st {
			out.docs = append(out.docs, d)
		}
	}
	return out
}

// Close releases every document's payload (deleting spill files). It returns the
// joined errors and is safe to call more than once.
func (s *Set) Close() error {
	var errs []error
	for _, d := range s.docs {
		if err := d.Release(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
