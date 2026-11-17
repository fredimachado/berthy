// Copyright 2013 @atotto. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package clipboard read/write on clipboard
package clipboard

// ReadAll read string from clipboard
func ReadAll() (string, error) {
	return readAll()
}

// WriteAll write string to clipboard
func WriteAll(text string) error {
	return writeAll(text)
}

// Unsupported is set true by the first ReadAll or WriteAll on Unix systems when no clipboard tool is found, to help callers
// decide whether or not to offer clipboard options. It is false before that first call.
var Unsupported bool
