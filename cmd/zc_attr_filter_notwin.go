//go:build !windows
// +build !windows

// Copyright © Microsoft <wastore@microsoft.com>
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package cmd

type attrFilter struct{}

func (f *attrFilter) DoesSupportThisOS() (msg string, supported bool) {
	msg = "'include-attributes' and 'exclude-attributes' are not supported on this OS. Abort."
	supported = false
	return
}

func (f *attrFilter) AppliesOnlyToFiles() bool {
	return true
}

func (f *attrFilter) DoesPass(storedObject StoredObject) bool {
	// ignore this option on Unix systems
	return true
}

func buildAttrFilters(attributes []string, fullPath string, resultIfMatch bool) []ObjectFilter {
	// ignore this option on Unix systems
	filters := make([]ObjectFilter, 0)
	if len(attributes) > 0 {
		filters = append(filters, &attrFilter{})
	}
	return filters
}
