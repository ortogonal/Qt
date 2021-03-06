// Copyright (c) 2014, Google Inc.
//
// Permission to use, copy, modify, and/or distribute this software for any
// purpose with or without fee is hereby granted, provided that the above
// copyright notice and this permission notice appear in all copies.
//
// THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
// WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
// MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR ANY
// SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
// WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN ACTION
// OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF OR IN
// CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE. */

package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// ssl.h reserves values 1000 and above for error codes corresponding to
// alerts. If automatically assigned reason codes exceed this value, this script
// will error. This must be kept in sync with SSL_AD_REASON_OFFSET in ssl.h.
const reservedReasonCode = 1000

var resetFlag *bool = flag.Bool("reset", false, "If true, ignore current assignments and reassign from scratch")

func makeErrors(reset bool) error {
	dirName, err := os.Getwd()
	if err != nil {
		return err
	}

	lib := filepath.Base(dirName)
	headerPath, err := findHeader(lib + ".h")
	if err != nil {
		return err
	}
	sourcePath := lib + "_error.c"

	headerFile, err := os.Open(headerPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("No header %s. Run in the right directory or touch the file.", headerPath)
		}

		return err
	}

	prefix := strings.ToUpper(lib)
	functions, reasons, err := parseHeader(prefix, headerFile)
	headerFile.Close()

	if reset {
		err = nil
		functions = make(map[string]int)
		// Retain any reason codes above reservedReasonCode.
		newReasons := make(map[string]int)
		for key, value := range reasons {
			if value >= reservedReasonCode {
				newReasons[key] = value
			}
		}
		reasons = newReasons
	}

	if err != nil {
		return err
	}

	dir, err := os.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()

	filenames, err := dir.Readdirnames(-1)
	if err != nil {
		return err
	}

	for _, name := range filenames {
		if !strings.HasSuffix(name, ".c") || name == sourcePath {
			continue
		}

		if err := addFunctionsAndReasons(functions, reasons, name, prefix); err != nil {
			return err
		}
	}

	assignNewValues(functions, -1)
	assignNewValues(reasons, reservedReasonCode)

	headerFile, err = os.Open(headerPath)
	if err != nil {
		return err
	}
	defer headerFile.Close()

	newHeaderFile, err := os.OpenFile(headerPath+".tmp", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		return err
	}
	defer newHeaderFile.Close()

	if err := writeHeaderFile(newHeaderFile, headerFile, prefix, functions, reasons); err != nil {
		return err
	}
	os.Rename(headerPath+".tmp", headerPath)

	sourceFile, err := os.OpenFile(sourcePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	fmt.Fprintf(sourceFile, `/* Copyright (c) 2014, Google Inc.
 *
 * Permission to use, copy, modify, and/or distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR ANY
 * SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN ACTION
 * OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF OR IN
 * CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE. */

#include <openssl/err.h>

#include <openssl/%s.h>

const ERR_STRING_DATA %s_error_string_data[] = {
`, lib, prefix)
	outputStrings(sourceFile, lib, typeFunctions, functions)
	outputStrings(sourceFile, lib, typeReasons, reasons)

	sourceFile.WriteString("  {0, NULL},\n};\n")

	return nil
}

func findHeader(basename string) (path string, err error) {
	includeDir := filepath.Join("..", "include")

	fi, err := os.Stat(includeDir)
	if err != nil && os.IsNotExist(err) {
		includeDir = filepath.Join("..", includeDir)
		fi, err = os.Stat(includeDir)
	}
	if err != nil {
		return "", errors.New("cannot find path to include directory")
	}
	if !fi.IsDir() {
		return "", errors.New("include node is not a directory")
	}
	return filepath.Join(includeDir, "openssl", basename), nil
}

type assignment struct {
	key   string
	value int
}

type assignmentsSlice []assignment

func (a assignmentsSlice) Len() int {
	return len(a)
}

func (a assignmentsSlice) Less(i, j int) bool {
	return a[i].value < a[j].value
}

func (a assignmentsSlice) Swap(i, j int) {
	a[i], a[j] = a[j], a[i]
}

func outputAssignments(w io.Writer, assignments map[string]int) {
	var sorted assignmentsSlice

	for key, value := range assignments {
		sorted = append(sorted, assignment{key, value})
	}

	sort.Sort(sorted)

	for _, assignment := range sorted {
		fmt.Fprintf(w, "#define %s %d\n", assignment.key, assignment.value)
	}
}

func parseDefineLine(line, lib string) (typ int, key string, value int, ok bool) {
	if !strings.HasPrefix(line, "#define ") {
		return
	}

	fields := strings.Fields(line)
	if len(fields) != 3 {
		return
	}

	funcPrefix := lib + "_F_"
	reasonPrefix := lib + "_R_"

	key = fields[1]
	switch {
	case strings.HasPrefix(key, funcPrefix):
		typ = typeFunctions
	case strings.HasPrefix(key, reasonPrefix):
		typ = typeReasons
	default:
		return
	}

	var err error
	if value, err = strconv.Atoi(fields[2]); err != nil {
		return
	}

	ok = true
	return
}

func writeHeaderFile(w io.Writer, headerFile io.Reader, lib string, functions, reasons map[string]int) error {
	var last []byte
	var haveLast, sawDefine bool
	newLine := []byte("\n")

	scanner := bufio.NewScanner(headerFile)
	for scanner.Scan() {
		line := scanner.Text()
		_, _, _, ok := parseDefineLine(line, lib)
		if ok {
			sawDefine = true
			continue
		}

		if haveLast {
			w.Write(last)
			w.Write(newLine)
		}

		if len(line) > 0 || !sawDefine {
			last = []byte(line)
			haveLast = true
		} else {
			haveLast = false
		}
		sawDefine = false
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	outputAssignments(w, functions)
	outputAssignments(w, reasons)
	w.Write(newLine)

	if haveLast {
		w.Write(last)
		w.Write(newLine)
	}

	return nil
}

const (
	typeFunctions = iota
	typeReasons
)

func outputStrings(w io.Writer, lib string, ty int, assignments map[string]int) {
	lib = strings.ToUpper(lib)
	prefixLen := len(lib + "_F_")

	keys := make([]string, 0, len(assignments))
	for key := range assignments {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		var pack string
		if ty == typeFunctions {
			pack = key + ", 0"
		} else {
			pack = "0, " + key
		}

		fmt.Fprintf(w, "  {ERR_PACK(ERR_LIB_%s, %s), \"%s\"},\n", lib, pack, key[prefixLen:])
	}
}

func assignNewValues(assignments map[string]int, reserved int) {
	max := 99

	for _, value := range assignments {
		if reserved >= 0 && value >= reserved {
			continue
		}
		if value > max {
			max = value
		}
	}

	max++

	for key, value := range assignments {
		if value == -1 {
			if reserved >= 0 && max >= reserved {
				// If this happens, try passing
				// -reset. Otherwise bump up reservedReasonCode.
				panic("Automatically-assigned values exceeded limit!")
			}
			assignments[key] = max
			max++
		}
	}
}

func handleDeclareMacro(line, join, macroName string, m map[string]int) {
	if i := strings.Index(line, macroName); i >= 0 {
		contents := line[i+len(macroName):]
		if i := strings.Index(contents, ")"); i >= 0 {
			contents = contents[:i]
			args := strings.Split(contents, ",")
			for i := range args {
				args[i] = strings.TrimSpace(args[i])
			}
			if len(args) != 2 {
				panic("Bad macro line: " + line)
			}
			token := args[0] + join + args[1]
			if _, ok := m[token]; !ok {
				m[token] = -1
			}
		}
	}
}

func addFunctionsAndReasons(functions, reasons map[string]int, filename, prefix string) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	prefix += "_"
	reasonPrefix := prefix + "R_"
	var currentFunction string

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()

		if len(line) > 0 && unicode.IsLetter(rune(line[0])) {
			/* Function start */
			fields := strings.Fields(line)
			for _, field := range fields {
				if i := strings.Index(field, "("); i != -1 {
					f := field[:i]
					// The return type of some functions is
					// a macro that contains a "(".
					if f == "STACK_OF" {
						continue
					}
					currentFunction = f
					for len(currentFunction) > 0 && currentFunction[0] == '*' {
						currentFunction = currentFunction[1:]
					}
					break
				}
			}
		}

		if strings.Contains(line, "OPENSSL_PUT_ERROR(") {
			functionToken := prefix + "F_" + currentFunction
			if _, ok := functions[functionToken]; !ok {
				functions[functionToken] = -1
			}
		}

		handleDeclareMacro(line, "_R_", "OPENSSL_DECLARE_ERROR_REASON(", reasons)
		handleDeclareMacro(line, "_F_", "OPENSSL_DECLARE_ERROR_FUNCTION(", functions)

		for len(line) > 0 {
			i := strings.Index(line, prefix)
			if i == -1 {
				break
			}

			line = line[i:]
			end := strings.IndexFunc(line, func(r rune) bool {
				return !(r == '_' || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
			})
			if end == -1 {
				end = len(line)
			}

			var token string
			token, line = line[:end], line[end:]

			switch {
			case strings.HasPrefix(token, reasonPrefix):
				if _, ok := reasons[token]; !ok {
					reasons[token] = -1
				}
			}
		}
	}

	return scanner.Err()
}

func parseHeader(lib string, file io.Reader) (functions, reasons map[string]int, err error) {
	functions = make(map[string]int)
	reasons = make(map[string]int)

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		typ, key, value, ok := parseDefineLine(scanner.Text(), lib)
		if !ok {
			continue
		}

		switch typ {
		case typeFunctions:
			functions[key] = value
		case typeReasons:
			reasons[key] = value
		default:
			panic("internal error")
		}
	}

	err = scanner.Err()
	return
}

func main() {
	flag.Parse()

	if err := makeErrors(*resetFlag); err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}
}
