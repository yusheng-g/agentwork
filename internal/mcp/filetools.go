package mcp

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// validatePath resolves p against workDir into a safe absolute path.
// Accepts both relative paths (joined with workDir) and absolute paths.
// Resolves symlinks but does NOT enforce workspace boundaries — the
// trust boundary is the daemon user (DESIGN.md 决策 4-8).
func validatePath(workDir, p string) (string, error) {
	var abs string
	if filepath.IsAbs(p) {
		abs = p
	} else {
		abs = filepath.Join(workDir, p)
	}
	abs, err := filepath.Abs(abs)
	if err != nil {
		return "", fmt.Errorf("invalid path: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err == nil {
		abs = real
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("invalid path: %w", err)
	}
	return abs, nil
}

// writeFilePreservingMode writes content, keeping the target's existing
// mode (so editing an executable keeps the exec bit). New files use 0644.
func writeFilePreservingMode(path string, content []byte) error {
	mode := os.FileMode(0644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode()
	}
	return os.WriteFile(path, content, mode)
}

func isBinary(data []byte) bool {
	n := len(data)
	if n > 512 {
		n = 512
	}
	for _, b := range data[:n] {
		if b == 0 {
			return true
		}
	}
	return false
}

func detectType(data []byte) string {
	n := len(data)
	if n > 64 {
		n = 64
	}
	for _, b := range data[:n] {
		if b < 9 || (b > 13 && b < 32) && b != 27 {
			return "binary data"
		}
	}
	if n >= 4 && data[0] == 0x7f && data[1] == 'E' && data[2] == 'L' && data[3] == 'F' {
		return "ELF executable"
	}
	return "unknown binary"
}

// addFileTools registers the workspace file tools: read_file, write_file,
// edit_file, list_dir, grep. Each operates on the executor's Worktree.
func addFileTools(srv *gmcp.Server, exec *Executor) {
	workDir := exec.Worktree

	// ── read_file ──

	type readArgs struct {
		Path  string `json:"path" jsonschema:"file path (absolute or relative to the workspace root)"`
		Line  any    `json:"line,omitempty" jsonschema:"start line (1-based, default: 1). Use with limit to read a specific range."`
		Limit any    `json:"limit,omitempty" jsonschema:"max lines to read (default: all remaining)"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "read_file",
		Description: "Read a file from the workspace. Use line+limit to read a specific line range — combine with grep to locate a line number first, then read the surrounding context.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args readArgs) (*gmcp.CallToolResult, any, error) {
		abs, err := validatePath(workDir, args.Path)
		if err != nil {
			return nil, nil, err
		}
		info, err := os.Stat(abs)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil, fmt.Errorf("read_file: file not found: %s", args.Path)
			}
			return nil, nil, fmt.Errorf("read_file: %w", err)
		}

		file, err := os.Open(abs)
		if err != nil {
			return nil, nil, fmt.Errorf("read_file: %w", err)
		}
		defer file.Close()

		// Binary detection: peek first 512 bytes for null bytes.
		peek := make([]byte, 512)
		n, _ := file.Read(peek)
		if n > 0 && isBinary(peek[:n]) {
			return &gmcp.CallToolResult{Content: []gmcp.Content{&gmcp.TextContent{Text: fmt.Sprintf("[binary file: %s, %d bytes, type: %s]", args.Path, info.Size(), detectType(peek[:n]))}}}, nil, nil
		}
		file.Seek(0, 0)

		lineNo := 1
		if p := parseInt64Ptr(args.Line); p != nil && *p > 0 {
			lineNo = int(*p)
		}
		limit := 0
		if p := parseInt64Ptr(args.Limit); p != nil && *p > 0 {
			limit = int(*p)
		}

		var out strings.Builder
		var lineNum, lineCount int
		var hitOffset bool
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // 1MB max line

		for scanner.Scan() {
			lineNum++
			if lineNum < lineNo {
				continue
			}
			hitOffset = true
			out.WriteString(scanner.Text())
			out.WriteByte('\n')
			lineCount++
			if limit > 0 && lineCount >= limit {
				break
			}
		}
		if err := scanner.Err(); err != nil {
			return nil, nil, fmt.Errorf("read_file: %w", err)
		}
		if !hitOffset {
			return &gmcp.CallToolResult{Content: []gmcp.Content{&gmcp.TextContent{Text: fmt.Sprintf("[line %d is beyond end of file (%d lines)]", lineNo, lineNum)}}}, nil, nil
		}

		text := out.String()
		if lineNo > 1 || limit > 0 {
			text = fmt.Sprintf("[lines %d-%d, %d total, %d bytes]:\n", lineNo, lineNo+lineCount-1, lineNum, info.Size()) + text
		}
		return &gmcp.CallToolResult{Content: []gmcp.Content{&gmcp.TextContent{Text: text}}}, nil, nil
	})

	// ── write_file ──

	type writeArgs struct {
		Path    string `json:"path" jsonschema:"file path (absolute or relative to the workspace root)"`
		Content string `json:"content" jsonschema:"content to write to the file"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "write_file",
		Description: "Write content to a file. Creates parent directories as needed. Do NOT use shell redirection for file writes.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args writeArgs) (*gmcp.CallToolResult, any, error) {
		const maxSize = 10 * 1024 * 1024 // 10MB
		if len(args.Content) > maxSize {
			return nil, nil, fmt.Errorf("write_file: content too large (%d bytes, max %d)", len(args.Content), maxSize)
		}
		abs, err := validatePath(workDir, args.Path)
		if err != nil {
			return nil, nil, err
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return nil, nil, fmt.Errorf("write_file: %w", err)
		}
		if err := writeFilePreservingMode(abs, []byte(args.Content)); err != nil {
			return nil, nil, fmt.Errorf("write_file: %w", err)
		}
		info, _ := os.Stat(abs)
		size := int64(0)
		if info != nil {
			size = info.Size()
		}
		return &gmcp.CallToolResult{Content: []gmcp.Content{&gmcp.TextContent{Text: fmt.Sprintf("Wrote %s (%d bytes)", args.Path, size)}}}, nil, nil
	})

	// ── edit_file ──

	type editArgs struct {
		Path       string `json:"path" jsonschema:"file path (absolute or relative to the workspace root)"`
		OldText    string `json:"old_text" jsonschema:"text to find and replace"`
		NewText    string `json:"new_text" jsonschema:"replacement text"`
		ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"replace all occurrences (default: false)"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "edit_file",
		Description: "Replace a string in a file. Finds old_text and replaces it with new_text. When replace_all is false (default), only the first match is replaced. Returns an error when old_text is not unique — use replace_all or make old_text more specific.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args editArgs) (*gmcp.CallToolResult, any, error) {
		abs, err := validatePath(workDir, args.Path)
		if err != nil {
			return nil, nil, err
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil, fmt.Errorf("edit_file: file not found: %s", args.Path)
			}
			return nil, nil, fmt.Errorf("edit_file: %w", err)
		}
		content := string(data)
		count := strings.Count(content, args.OldText)
		if count == 0 {
			return nil, nil, fmt.Errorf("edit_file: old_text not found in %s", args.Path)
		}
		if !args.ReplaceAll && count > 1 {
			return nil, nil, fmt.Errorf("edit_file: old_text found %d times in %s — set replace_all to true or make old_text more specific", count, args.Path)
		}
		n := 1
		if args.ReplaceAll {
			n = count
		}
		newContent := strings.Replace(content, args.OldText, args.NewText, n)
		if err := writeFilePreservingMode(abs, []byte(newContent)); err != nil {
			return nil, nil, fmt.Errorf("edit_file: %w", err)
		}
		if args.ReplaceAll {
			return &gmcp.CallToolResult{Content: []gmcp.Content{&gmcp.TextContent{Text: fmt.Sprintf("Replaced %d occurrences in %s", count, args.Path)}}}, nil, nil
		}
		return &gmcp.CallToolResult{Content: []gmcp.Content{&gmcp.TextContent{Text: fmt.Sprintf("Replaced in %s", args.Path)}}}, nil, nil
	})

	// ── list_dir ──

	type listArgs struct {
		Path string `json:"path,omitempty" jsonschema:"directory path (default: workspace root)"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "list_dir",
		Description: "List files and directories at the given path (default: workspace root).",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args listArgs) (*gmcp.CallToolResult, any, error) {
		dir := workDir
		if args.Path != "" {
			abs, err := validatePath(workDir, args.Path)
			if err != nil {
				return nil, nil, err
			}
			dir = abs
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, nil, fmt.Errorf("list_dir: %w", err)
		}
		type fileEntry struct {
			Name  string
			Size  int64
			IsDir bool
		}
		var files []fileEntry
		for _, e := range entries {
			info, err := e.Info()
			size := int64(0)
			if err == nil {
				size = info.Size()
			}
			files = append(files, fileEntry{e.Name(), size, e.IsDir()})
		}
		sort.Slice(files, func(i, j int) bool {
			if files[i].IsDir != files[j].IsDir {
				return files[i].IsDir
			}
			return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name)
		})
		var b strings.Builder
		if args.Path != "" {
			b.WriteString(args.Path + ":\n")
		}
		for _, f := range files {
			if f.IsDir {
				b.WriteString(fmt.Sprintf("  %s/\n", f.Name))
			} else {
				b.WriteString(fmt.Sprintf("  %s  (%d)\n", f.Name, f.Size))
			}
		}
		if len(files) == 0 {
			b.WriteString("  (empty)\n")
		}
		return &gmcp.CallToolResult{Content: []gmcp.Content{&gmcp.TextContent{Text: b.String()}}}, nil, nil
	})

	// ── grep ──

	type grepArgs struct {
		Pattern string `json:"pattern" jsonschema:"text or regex pattern to search for (case-sensitive unless (?i) flag used)"`
		Path    string `json:"path,omitempty" jsonschema:"subdirectory to search (default: workspace root)"`
		Glob    string `json:"glob,omitempty" jsonschema:"file pattern filter, e.g. '*.go' or '*.{go,md}' (default: all text files)"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "grep",
		Description: "Search for a pattern in workspace files. Returns matching file paths, line numbers, and content. Use for finding usages, definitions, or patterns in the codebase.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args grepArgs) (*gmcp.CallToolResult, any, error) {
		searchDir := workDir
		if args.Path != "" {
			abs, err := validatePath(workDir, args.Path)
			if err != nil {
				return nil, nil, err
			}
			searchDir = abs
		}
		re, err := regexp.Compile(args.Pattern)
		if err != nil {
			return nil, nil, fmt.Errorf("grep: invalid pattern: %w", err)
		}
		const maxFileSize = 1 * 1024 * 1024 // 1MB
		type fileMatch struct {
			file  string
			lines []string
		}
		var fileMatches []fileMatch
		total := 0
		err = filepath.WalkDir(searchDir, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if ctx.Err() != nil {
				return filepath.SkipAll
			}
			if d.IsDir() {
				base := d.Name()
				if strings.HasPrefix(base, ".") && base != "." {
					return filepath.SkipDir
				}
				return nil
			}
			if args.Glob != "" {
				matched, _ := filepath.Match(args.Glob, d.Name())
				if !matched {
					return nil
				}
			}
			info, err := d.Info()
			if err != nil || info.Size() > maxFileSize {
				return nil
			}
			rel, _ := filepath.Rel(workDir, path)
			found, err := grepFile(path, re)
			if err != nil || len(found) == 0 {
				return nil
			}
			fileMatches = append(fileMatches, fileMatch{rel, found})
			total += len(found)
			return nil
		})
		if err != nil {
			return nil, nil, fmt.Errorf("grep: %w", err)
		}
		if total == 0 {
			return &gmcp.CallToolResult{Content: []gmcp.Content{&gmcp.TextContent{Text: "No matches found."}}}, nil, nil
		}
		var b strings.Builder
		b.WriteString(fmt.Sprintf("%d matches:\n", total))
		for _, fm := range fileMatches {
			b.WriteString(fm.file + ":\n")
			for _, ln := range fm.lines {
				b.WriteString("  ")
				b.WriteString(ln)
				b.WriteByte('\n')
			}
		}
		return &gmcp.CallToolResult{Content: []gmcp.Content{&gmcp.TextContent{Text: b.String()}}}, nil, nil
	})
}

// grepFile scans one file for re and returns matching lines formatted as
// "lineNum: content". The file is opened and closed within this call.
func grepFile(path string, re *regexp.Regexp) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 1MB max line
	var matches []string
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		if re.MatchString(line) {
			matches = append(matches, fmt.Sprintf("%d: %s", lineNum, line))
		}
	}
	return matches, nil
}
