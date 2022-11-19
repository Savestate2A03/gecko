package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
)

var compileWaitGroup sync.WaitGroup
var toCompileCount uint32
var compileJobs []compileJob
var jobMtx sync.Mutex

type symbolInfo struct {
	name    string
	linePos int
}

type labelInfo struct {
	name    string
	num     int
	linePos int
}

type compileResponse struct {
	code    []byte
	address string
}

type compileJob struct {
	inputFile  string
	addressExp string
	response   chan compileResponse
}

func execBatchCompile(jobs []compileJob) {
	const asCmdLinux string = "powerpc-eabi-as"
	const objcopyCmdLinux string = "powerpc-eabi-objcopy"

	outputFilePath := path.Join(argConfig.ProjectRoot, "compiled.elf")
	compileWaitGroup.Add(1)
	defer func() {
		defer compileWaitGroup.Done()
		os.Remove(outputFilePath)
	}()

	// Set base args
	args := []string{"-a32", "-mbig", "-mregnames", "-mgekko", "-W"}

	// If defsym is defined, add it to the args
	if argConfig.DefSym != "" {
		args = append(args, "-defsym", argConfig.DefSym)
	}

	args = append(args, "-I", argConfig.ProjectRoot)

	// Add local paths to look at when resolving includes
	for _, job := range jobs {
		file := job.inputFile
		fileDir := filepath.Dir(file)
		args = append(args, "-I", fileDir)
	}

	// Set output file
	args = append(args, "-o", outputFilePath)

	// Iterate through jobs, create temp files, and add them to the files to assemble
	for idx, job := range jobs {
		file := job.inputFile
		fileExt := filepath.Ext(file)
		compileFilePath := file[0:len(file)-len(fileExt)] + ".asmtemp"

		compileWaitGroup.Add(1)
		defer func() {
			defer compileWaitGroup.Done()
			os.Remove(compileFilePath)
		}()

		buildTempAsmFile(file, job.addressExp, compileFilePath, fmt.Sprintf("file%d", idx))
		args = append(args, compileFilePath)
	}

	cmd := exec.Command(asCmdLinux, args...)

	output, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("Failed to compile files")
		fmt.Printf("%s", output)
		panic("as failure")
	}

	args = []string{outputFilePath}
	for idx, job := range jobs {
		file := job.inputFile
		fileExt := filepath.Ext(file)
		codeFilePath := file[0:len(file)-len(fileExt)] + ".out"

		compileWaitGroup.Add(1)
		defer func() {
			defer compileWaitGroup.Done()
			os.Remove(codeFilePath)
		}()

		args = append(args, "--dump-section", fmt.Sprintf("file%d=%s", idx, codeFilePath))
	}

	cmd = exec.Command(objcopyCmdLinux, args...)
	output, err = cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("Failed to pull extract code sections\n")
		fmt.Printf("%s", output)
		panic("objcopy failure")
	}

	for _, job := range jobs {
		file := job.inputFile
		fileExt := filepath.Ext(file)
		codeFilePath := file[0:len(file)-len(fileExt)] + ".out"
		contents, err := ioutil.ReadFile(codeFilePath)
		if err != nil {
			log.Panicf("Failed to read compiled file %s\n%s\n", codeFilePath, err.Error())
		}

		code := contents[:len(contents)-4]
		address := contents[len(contents)-4:]
		if address[0] != 0x80 && address[0] != 0x81 {
			log.Panicf("Injection address in file %s evaluated to a value that does not start with 0x80 or 0x81, probably an invalid address\n", file)
		}

		job.response <- compileResponse{code: code, address: fmt.Sprintf("%x", address)}
	}
}

func batchCompile(file, addressExp string) ([]byte, string) {
	// return compile(file, addressExp)

	c := make(chan compileResponse)
	jobMtx.Lock()
	compileJobs = append(compileJobs, compileJob{
		inputFile:  file,
		addressExp: addressExp,
		response:   c,
	})

	if len(compileJobs) >= int(toCompileCount) {
		go execBatchCompile(compileJobs)
	}
	jobMtx.Unlock()

	result := <-c
	return result.code, result.address
}

func compile(file, addressExp string) ([]byte, string) {
	fileExt := filepath.Ext(file)
	outputFilePath := file[0:len(file)-len(fileExt)] + ".out"
	compileFilePath := file[0:len(file)-len(fileExt)] + ".asmtemp"

	// Clean up files
	defer os.Remove(outputFilePath)
	defer os.Remove(compileFilePath)

	// First we are gonna load all the data from file and write it into temp file
	// Technically this shouldn't be necessary but for some reason if the last line
	// or the asm file has one of more spaces at the end and no new line, the last
	// instruction is ignored and not compiled
	buildTempAsmFile(file, addressExp, compileFilePath, "")

	fileDir := filepath.Dir(file)

	const asCmdLinux string = "powerpc-eabi-as"
	const objcopyCmdLinux string = "powerpc-eabi-objcopy"

	// Set base args
	args := []string{"-a32", "-mbig", "-mregnames", "-mgekko"}

	// If defsym is defined, add it to the args
	if argConfig.DefSym != "" {
		args = append(args, "-defsym", argConfig.DefSym)
	}

	// Add paths to look at when resolving includes
	args = append(args, "-I", fileDir, "-I", argConfig.ProjectRoot)

	// Set output file
	args = append(args, "-o", outputFilePath, compileFilePath)

	cmd := exec.Command(asCmdLinux, args...)

	output, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("Failed to compile file: %s\n", file)
		fmt.Printf("%s", output)
		panic("as failure")
	}

	contents, err := ioutil.ReadFile(outputFilePath)
	if err != nil {
		log.Panicf("Failed to read compiled file %s\n%s\n", file, err.Error())
	}

	// This gets the index right before the value of the last .set
	addressEndIndex := bytes.LastIndex(contents, []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xFF, 0xF1, 0x00})
	address := contents[addressEndIndex-4 : addressEndIndex]
	if address[0] != 0x80 {
		log.Panicf("Injection address in file %s evaluated to a value that does not start with 0x80, probably an invalid address\n", file)
	}

	cmd = exec.Command(objcopyCmdLinux, "-O", "binary", outputFilePath, outputFilePath)
	output, err = cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("Failed to pull out .text section: %s\n", file)
		fmt.Printf("%s", output)
		panic("objcopy failure")
	}
	contents, err = ioutil.ReadFile(outputFilePath)
	if err != nil {
		log.Panicf("Failed to read compiled file %s\n%s\n", file, err.Error())
	}
	return contents, fmt.Sprintf("%x", address)
}

func splitSymbols(s string) []string {
	splitter := func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '_'
	}
	return strings.FieldsFunc(s, splitter)
}

func splitAny(s string, seps string) []string {
	splitter := func(r rune) bool {
		return strings.ContainsRune(seps, r)
	}
	return strings.FieldsFunc(s, splitter)
}

func isolateLabelNames(asmContents []byte) []byte {
	// Start logic to isolate label names
	// First we're going to extract all label positions as well as replace them with a number
	// based label which functions as a local label. This will prevent errors from using
	// the same label name in multiple files
	lines := strings.Split(string(asmContents), "\n")
	labels := map[string]labelInfo{}
	newLines := []string{}
	labelIdx := 100 // Start at 100 because hopefully no macros will use labels that high
	for lineNum, line := range lines {
		// Remove any comments
		commentSplit := strings.Split(line, "#")
		if len(commentSplit) == 0 {
			newLines = append(newLines, line)
			continue
		}

		trimmed := strings.TrimSpace(commentSplit[0])
		isLabel := len(trimmed) > 0 && trimmed[len(trimmed)-1] == ':'
		if !isLabel {
			newLines = append(newLines, trimmed)
			continue
		}

		name := trimmed[:len(trimmed)-1]
		labels[name] = labelInfo{name, labelIdx, lineNum}
		newLines = append(newLines, fmt.Sprintf("%d:", labelIdx))
		labelIdx += 1
	}

	// Now let's convert all the branch instructions we can find to use the local labels
	// instead of the original label names
	// TODO: It might be possible to throw errors here if referencing a label that doesn't exist
	// TODO: I didn't do it yet because currently instructions like `branchl r12, ...` might
	// TODO: trigger the easy form of detection. We'd probably have to detect all possible branch
	// TODO: instructions in order to do this
	finalLines := []string{}
	for lineNum, line := range newLines {
		parts := splitAny(line, " \t")
		if len(parts) == 0 {
			finalLines = append(finalLines, line)
			continue
		}

		label := parts[len(parts)-1]
		li, labelExists := labels[label]
		isBranch := len(parts) >= 2 && line[0] == 'b' && labelExists
		if !isBranch {
			finalLines = append(finalLines, line)
			continue
		}

		dir := "f"
		if lineNum > li.linePos {
			dir = "b"
		}

		parts[len(parts)-1] = fmt.Sprintf("%d%s", li.num, dir)
		finalLines = append(finalLines, strings.Join(parts, " "))
	}

	return []byte(strings.Join(finalLines, "\r\n"))
}

// func isolateSymbolNames(asmContents []byte, section string) []byte {
// 	lines := strings.Split(string(asmContents), "\n")
// 	symbolMap := map[string][]symbolInfo{}
// 	newLines := []string{}
// 	for idx, line := range lines {
// 		parts := splitAny(line, " \t,")
// 		if len(parts) == 0 {
// 			newLines = append(newLines, line)
// 			continue
// 		}

// 		isSet := parts[0] == ".set" && len(parts) >= 3
// 		if !isSet {
// 			newLines = append(newLines, line)
// 			continue
// 		}

// 		symbolMap[parts[1]] = fmt.Sprintf("__%s_symbol_%d", section, idx)
// 	}
// }

func buildTempAsmFile(sourceFilePath, addressExp, targetFilePath, section string) {
	asmContents, err := ioutil.ReadFile(sourceFilePath)
	if err != nil {
		log.Panicf("Failed to read asm file: %s\n%s\n", sourceFilePath, err.Error())
	}

	// If section provided, we need to take some precautions to isolate the code from others
	if section != "" {
		// Add the section label at the top so the code can be extracted individually
		asmContents = append([]byte(fmt.Sprintf(".section %s\r\n", section)), asmContents...)
		asmContents = isolateLabelNames(asmContents)
		// asmContents = isolateSymbolNames(asmContents, section)
	}

	// Add new line before .set for address
	asmContents = append(asmContents, []byte("\r\n")...)

	// Add .set to get file injection address
	setLine := fmt.Sprintf(".long %s\r\n", addressExp)
	asmContents = append(asmContents, []byte(setLine)...)

	// Explicitly add a new line at the end of the file, which should prevent line skip
	asmContents = append(asmContents, []byte("\r\n")...)
	err = ioutil.WriteFile(targetFilePath, asmContents, 0644)
	if err != nil {
		log.Panicf("Failed to write temporary asm file\n%s\n", err.Error())
	}
}
