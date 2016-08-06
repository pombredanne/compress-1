// Copyright 2015, Joe Tsai. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE.md file.

// Package flate implements the DEFLATE compressed data format,
// described in RFC 1951.
package flate

import (
	"fmt"

	"github.com/dsnet/compress/internal/errors"
)

const (
	maxHistSize = 1 << 15
	endBlockSym = 256
)

func errorf(c int, f string, a ...interface{}) error {
	return errors.Error{c, "flate", fmt.Sprintf(f, a...)}
}

// errWrap converts a lower-level errors.Error to be one from this package.
// The replaceCode passed in will be used to replace the code for any errors
// with the errors.Invalid code.
//
// For the Reader, set this to errors.Corrupted.
// For the Writer, set this to errors.Internal.
func errWrap(err error, replaceCode int) error {
	if cerr, ok := err.(errors.Error); ok {
		if errors.IsInvalid(cerr) {
			cerr.Code = replaceCode
		}
		err = errorf(cerr.Code, "%s", cerr.Msg)
	}
	return err
}

var (
	errCorrupted = errorf(errors.Corrupted, "")
	errClosed    = errorf(errors.Closed, "")
)
