package syncer

import (
	"fmt"
	"strings"
)

// maxLCSCells caps the O(n*m) line diff; larger inputs fall back to a
// single whole-file hunk.
const maxLCSCells = 16_000_000

type diffOp struct {
	kind byte // ' ', '-', '+'
	line string
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func lineOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	// Trim common prefix/suffix to keep the table small.
	pre := 0
	for pre < n && pre < m && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < n-pre && suf < m-pre && a[n-1-suf] == b[m-1-suf] {
		suf++
	}
	var ops []diffOp
	for i := 0; i < pre; i++ {
		ops = append(ops, diffOp{' ', a[i]})
	}
	ma, mb := a[pre:n-suf], b[pre:m-suf]
	if len(ma)*len(mb) > maxLCSCells {
		for _, l := range ma {
			ops = append(ops, diffOp{'-', l})
		}
		for _, l := range mb {
			ops = append(ops, diffOp{'+', l})
		}
	} else {
		ops = append(ops, lcsOps(ma, mb)...)
	}
	for i := n - suf; i < n; i++ {
		ops = append(ops, diffOp{' ', a[i]})
	}
	return ops
}

func lcsOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	t := make([][]int32, n+1)
	for i := range t {
		t[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				t[i][j] = t[i+1][j+1] + 1
			} else if t[i+1][j] >= t[i][j+1] {
				t[i][j] = t[i+1][j]
			} else {
				t[i][j] = t[i][j+1]
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case t[i+1][j] >= t[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}

// UnifiedDiff renders a unified diff (3 lines of context) between two
// texts. It returns "" when they are equal. CRLF is shown as LF.
func UnifiedDiff(fromName, toName, a, b string) string {
	a = string(NormalizeLF([]byte(a)))
	b = string(NormalizeLF([]byte(b)))
	if a == b {
		return ""
	}
	ops := lineOps(splitLines(a), splitLines(b))
	const ctx = 3
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", fromName, toName)
	// Positions in a/b for each op index.
	type pos struct{ ai, bi int }
	positions := make([]pos, len(ops)+1)
	ai, bi := 0, 0
	for k, op := range ops {
		positions[k] = pos{ai, bi}
		switch op.kind {
		case ' ':
			ai++
			bi++
		case '-':
			ai++
		case '+':
			bi++
		}
	}
	positions[len(ops)] = pos{ai, bi}

	k := 0
	for k < len(ops) {
		for k < len(ops) && ops[k].kind == ' ' {
			k++
		}
		if k >= len(ops) {
			break
		}
		start := k - ctx
		if start < 0 {
			start = 0
		}
		end := k
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			run := end
			for run < len(ops) && ops[run].kind == ' ' {
				run++
			}
			if run >= len(ops) || run-end > 2*ctx {
				end += ctx
				if end > run {
					end = run
				}
				break
			}
			end = run
		}
		if end > len(ops) {
			end = len(ops)
		}
		aStart, bStart := positions[start].ai, positions[start].bi
		aLen := positions[end].ai - aStart
		bLen := positions[end].bi - bStart
		fmt.Fprintf(&sb, "@@ -%s +%s @@\n", hunkRange(aStart, aLen), hunkRange(bStart, bLen))
		for _, op := range ops[start:end] {
			sb.WriteByte(op.kind)
			sb.WriteString(op.line)
			if !strings.HasSuffix(op.line, "\n") {
				sb.WriteString("\n\\ No newline at end of file\n")
			}
		}
		k = end
	}
	return sb.String()
}

func hunkRange(start, length int) string {
	if length == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	if length == 1 {
		return fmt.Sprintf("%d", start+1)
	}
	return fmt.Sprintf("%d,%d", start+1, length)
}

// ChangeDiff renders the diff for one planned change.
func ChangeDiff(c Change) string {
	from, to := "a/"+c.Path, "b/"+c.Path
	if !c.BeforeExists {
		from = "/dev/null"
	}
	if c.Action == ActDelete {
		to = "/dev/null"
	}
	return UnifiedDiff(from, to, string(c.Before), string(c.After))
}
