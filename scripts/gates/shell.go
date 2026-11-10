package main

import (
	"path"
	"slices"
	"strings"
)

// One reader for a go command on a shell command line, shared by the
// pins and parity gates: what a line runs is what the shell runs, so a
// flag, a quote or a substitution cannot hide a package from either.

// goRun is one `go run` command: the package it runs, such as
// example.com/tool@v1.2.3 or ./scripts/gates, and the words after it.
type goRun struct {
	pkg  string
	args []string
}

// goValueFlags are the flags the go command reads its next word for, as
// go1.27.3's `go help build`, `go help run`, `go help vet` and `go tool vet
// -flags` list them: the build flags and build's -o, run's -exec, vet's
// -vettool and the vet tool's own
// (https://pkg.go.dev/cmd/go#hdr-Compile_packages_and_dependencies). The
// help leaves out three debug flags that take a value too
// (https://github.com/golang/go/blob/go1.27.3/src/cmd/go/internal/work/build.go#L358-L361).
// c is the vet tool's lines of context. `go test -c` is a boolean, so no
// gate reads a go test's words through this map.
var goValueFlags = map[string]bool{
	"C": true, "p": true, "covermode": true, "coverpkg": true, "asmflags": true, "buildmode": true,
	"compiler": true, "gccgoflags": true, "gcflags": true, "installsuffix": true, "ldflags": true,
	"mod": true, "modfile": true, "overlay": true, "pgo": true, "pkgdir": true, "tags": true,
	"toolexec": true, "o": true, "exec": true, "vettool": true, "c": true, "printf.funcs": true,
	"printfuncs": true, "unusedfuncs": true, "unusedresult.funcs": true, "unusedresult.stringmethods": true,
	"unusedstringmethods": true, "debug-actiongraph": true, "debug-runtime-trace": true, "debug-trace": true,
}

// goRunsIn is each `go run` a command line holds, its flags skipped, so
// `go run -mod=mod tool@v1` runs tool@v1.
func goRunsIn(line string) []goRun { return goRunsOf(shellSplit(line)) }

// goRunsOf is goRunsIn over a line already split.
func goRunsOf(words []shellWord) []goRun {
	var runs []goRun
	for _, c := range goCommandsOf(words) {
		if rest := goAfterFlags(c.args); c.sub == "run" && len(rest) > 0 {
			runs = append(runs, goRun{pkg: rest[0], args: rest[1:]})
		}
	}
	return runs
}

// goAfterFlags is a go command's words after its flags, a flag that takes
// a value skipping that value too: the packages, or for `go run` its one
// package and the program's arguments.
func goAfterFlags(words []string) []string {
	i := 0
	for i < len(words) && strings.HasPrefix(words[i], "-") {
		name, _, inline := strings.Cut(strings.TrimLeft(words[i], "-"), "=")
		i++
		if !inline && goValueFlags[name] {
			i++
		}
	}
	return words[min(i, len(words)):]
}

// goCommand is one `go <sub>` on a command line: the subcommand, such as
// run or vet, and the words after it to the end of its command.
type goCommand struct {
	sub  string
	args []string
}

// goCommands is each go command a command line holds, the line split once
// as the shell splits it. A command substitution, backticks and a subshell
// hold commands of their own, so `OUT=$(go run x@v1)` runs x@v1.
func goCommands(line string) []goCommand { return goCommandsOf(shellSplit(line)) }

// goCommandsOf is goCommands over a line already split. A redirection is
// no word of the command it sits in.
func goCommandsOf(words []shellWord) []goCommand {
	words = slices.DeleteFunc(slices.Clone(words), func(w shellWord) bool { return w.redir })
	var out []goCommand
	for i := 0; i+1 < len(words); i++ {
		// The go command by any path, such as $GOROOT/bin/go.
		if path.Base(words[i].text) != "go" || words[i+1].op {
			continue
		}
		end := i + 2
		var args []string
		for ; end < len(words) && !words[end].op; end++ {
			args = append(args, words[end].text)
		}
		out = append(out, goCommand{sub: words[i+1].text, args: args})
		i = end
	}
	return out
}

// shellWord is one word of a command line, op when it is an operator that
// ends a command rather than text, and redir when it is a redirection.
type shellWord struct {
	text  string
	op    bool
	redir bool
}

// shellSplit splits a command line into words as a POSIX shell does,
// expanding nothing: blanks separate words, quotes group and are dropped,
// a backslash outside single quotes keeps the next character, a run of ;,
// & or | is an operator, and a # that starts a word starts a comment
// (https://pubs.opengroup.org/onlinepubs/9799919799/utilities/V3_chap02.html#tag_19_03).
// $(, (, ) and a backtick are operators too, since a command substitution
// or a subshell holds commands of its own
// (https://pubs.opengroup.org/onlinepubs/9799919799/utilities/V3_chap02.html#tag_19_06_03).
// A redirection, such as 2>&1, < in or bash's &>log, is one word with its
// target, and neither a word of its command nor an operator that ends it
// (https://pubs.opengroup.org/onlinepubs/9799919799/utilities/V3_chap02.html#tag_19_07,
// https://www.gnu.org/software/bash/manual/bash.html#Redirecting-Standard-Output-and-Standard-Error).
func shellSplit(line string) []shellWord {
	var s shellSplitter
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case s.quote == '\'':
			s.inQuote(c)
		case s.escaped(line, i):
			i++
			s.add(line[i])
		case s.substitution(line, &i):
		case s.quote == '"':
			s.inQuote(c)
		case strings.IndexByte(`'"`, c) >= 0:
			s.quote, s.inWord = c, true
		case c == '#' && !s.inWord:
			return s.finish()
		case strings.IndexByte(" \t", c) >= 0:
			s.flush()
		case s.redirection(line, &i):
		case s.operatorRun(line, &i):
		default:
			s.add(c)
		}
	}
	return s.finish()
}

// shellSplitter is shellSplit's state: the words read, the one being read,
// the quote the reader is in, the substitutions and subshells around it,
// each with the character that ends it and the quote it was opened in, and
// a redirection's operator waiting for its target.
type shellSplitter struct {
	words    []shellWord
	word     strings.Builder
	inWord   bool
	quote    byte
	opened   []struct{ end, quote byte }
	redirect string
}

// inQuote reads c inside a quote, whose own character ends it.
func (s *shellSplitter) inQuote(c byte) {
	if c == s.quote {
		s.quote = 0
	} else {
		s.word.WriteByte(c)
	}
}

// escaped reports whether line[i] is a backslash that keeps the next
// character: any outside quotes, and in double quotes one before ", \, $
// or a backtick.
func (s *shellSplitter) escaped(line string, i int) bool {
	return line[i] == '\\' && i+1 < len(line) && (s.quote == 0 || strings.IndexByte("\"\\$`", line[i+1]) >= 0)
}

// add appends c to the word being read.
func (s *shellSplitter) add(c byte) {
	s.word.WriteByte(c)
	s.inWord = true
}

// operatorRun reads a run of one of ;, & or | at line[*i] as one
// operator, moving *i to its last character, and reports whether there
// was one.
func (s *shellSplitter) operatorRun(line string, i *int) bool {
	c := line[*i]
	if strings.IndexByte(";&|", c) < 0 {
		return false
	}
	j := *i
	for j+1 < len(line) && line[j+1] == c {
		j++
	}
	s.operator(line[*i : j+1])
	*i = j
	return true
}

// flush ends the word being read, the target of a waiting redirection
// when there is one.
func (s *shellSplitter) flush() {
	if s.inWord {
		w := shellWord{text: s.word.String()}
		if s.redirect != "" {
			w = shellWord{text: s.redirect + w.text, redir: true}
			s.redirect = ""
		}
		s.words = append(s.words, w)
		s.word.Reset()
		s.inWord = false
	}
}

// finish ends the line: the word being read, and a redirection that never
// got its target, as it is.
func (s *shellSplitter) finish() []shellWord {
	s.flush()
	if s.redirect != "" {
		s.words = append(s.words, shellWord{text: s.redirect, redir: true})
		s.redirect = ""
	}
	return s.words
}

func (s *shellSplitter) operator(op string) {
	s.words = append(s.finish(), shellWord{text: op, op: true})
}

// redirection reads a redirection operator at line[*i], moving *i to its
// last character, and reports whether there was one. The operator waits
// for the next word as its target. A word of digits alone and unquoted
// before it is the file descriptor it redirects, part of the redirection
// (https://pubs.opengroup.org/onlinepubs/9799919799/utilities/V3_chap02.html#tag_19_10_01).
func (s *shellSplitter) redirection(line string, i *int) bool {
	c := line[*i]
	if c != '<' && c != '>' && !strings.HasPrefix(line[*i:], "&>") {
		return false
	}
	j := *i + 1
	for j < len(line) && strings.IndexByte("<>&|", line[j]) >= 0 {
		j++
	}
	op := line[*i:j]
	if fd := s.word.String(); s.inWord && s.redirect == "" && c != '&' && fd != "" &&
		strings.Trim(fd, "0123456789") == "" && strings.HasSuffix(line[:*i], fd) {
		op = fd + op
		s.word.Reset()
		s.inWord = false
	}
	s.finish()
	s.redirect, *i = op, j-1
	return true
}

// substitution reads the start or the end of a substitution or a subshell
// at line[*i], moving *i past a $(, and reports whether there was one. Its
// end goes back to the quote it was opened in.
func (s *shellSplitter) substitution(line string, i *int) bool {
	c, n := line[*i], len(s.opened)
	op, end := string(c), byte(')')
	switch {
	case n > 0 && c == s.opened[n-1].end && (c == '`' || s.quote == 0):
		s.operator(op)
		s.quote, s.opened = s.opened[n-1].quote, s.opened[:n-1]
		s.inWord = s.quote != 0
		return true
	case s.quote == 0 && c == ')':
		s.operator(op)
		return true
	case c == '`':
		end = '`'
	case strings.HasPrefix(line[*i:], "$("):
		op = "$("
		*i++
	case s.quote != 0 || c != '(':
		return false
	}
	s.operator(op)
	s.opened = append(s.opened, struct{ end, quote byte }{end, s.quote})
	s.quote = 0
	return true
}

// shellTexts is split words, operators and redirections included, as text.
func shellTexts(split []shellWord) []string {
	words := make([]string, 0, len(split))
	for _, w := range split {
		words = append(words, w.text)
	}
	return words
}

// shellListOps are the operators that end a command in a list
// (https://pubs.opengroup.org/onlinepubs/9799919799/utilities/V3_chap02.html#tag_19_09_03).
var shellListOps = map[string]bool{";": true, "&": true, "&&": true, "||": true}

// shellLists splits a line's words at its list operators into the commands
// they separate, an empty one dropped. A pipeline stays one command.
func shellLists(words []shellWord) [][]shellWord { return shellSplitAt(words, shellListOps) }

// shellSplitAt splits words at each operator in ops into the runs between
// them, an empty one dropped.
func shellSplitAt(words []shellWord, ops map[string]bool) [][]shellWord {
	var out [][]shellWord
	start := 0
	for i := 0; i <= len(words); i++ {
		if i < len(words) && (!words[i].op || !ops[words[i].text]) {
			continue
		}
		if i > start {
			out = append(out, words[start:i])
		}
		start = i + 1
	}
	return out
}
