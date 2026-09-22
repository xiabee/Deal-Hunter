package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 文档是这个项目的操作手册，而它的表格今晚被改坏过两次：一次在里程碑表中间留下
// 空行（表格从那儿断成两截），一次把某行的第三列整个丢掉。`grep '^|'` 两种都看不见。
// 这里按 GFM 的规则逐块核对：表头、分隔行、然后每一行的未转义竖线数必须与表头一致。
func TestMarkdownTablesAreWellFormed(t *testing.T) {
	dirs := []string{filepath.Join("..", ".."), filepath.Join("..", "..", "docs")}
	blocks := 0
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			lines := strings.Split(string(raw), "\n")
			for i := 0; i < len(lines); i++ {
				if !strings.HasPrefix(lines[i], "|") {
					continue
				}
				j := i
				for j < len(lines) && strings.HasPrefix(lines[j], "|") {
					j++
				}
				size := j - i
				if size < 2 {
					t.Errorf("%s:%d: 只有一行的表格块，多半是上一行被物理换行切断了", path, i+1)
					blocks++
					continue
				}
				blocks++
				head := pipes(lines[i])
				if !strings.HasPrefix(strings.ReplaceAll(lines[i+1], " ", ""), "|---") || pipes(lines[i+1]) != head {
					t.Errorf("%s:%d: 表头下面不是与表头对齐的分隔行（表格被截断或列数不一致）", path, i+1)
					continue
				}
				for k := i + 2; k < j; k++ {
					if got := pipes(lines[k]); got != head {
						t.Errorf("%s:%d: 这一行有 %d 个竖线，表头是 %d 个 —— 表格会断开", path, k+1, got, head)
					}
				}
				i = j - 1
			}
		}
	}
	if blocks < 6 {
		t.Fatalf("只找到 %d 个表格块，这个检查基本等于没跑", blocks)
	}
	t.Logf("checked %d markdown table blocks", blocks)
}

// pipes counts unescaped cell separators: a \| inside a cell is content, not a column.
func pipes(line string) int {
	n := 0
	for i := 0; i < len(line); i++ {
		if line[i] != '|' {
			continue
		}
		esc := 0
		for k := i - 1; k >= 0 && line[k] == '\\'; k-- {
			esc++
		}
		if esc%2 == 0 {
			n++
		}
	}
	return n
}
