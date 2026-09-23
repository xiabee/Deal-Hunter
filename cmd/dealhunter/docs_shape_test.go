package main

import (
	"os"
	"path/filepath"
	"regexp"
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

// 两份门禁孪生各写一遍是本项目的既定形状（Windows 跑 ci-local.ps1，Linux 与构建机跑
// ci-local.sh），代价就是"某条演练只接进一份"这种漂移：另一台上那一腿永远没人跑，
// 而 office 那份全绿看起来像全套过了。M48 那条正好是两态不同的（systemctl 前提只在
// Linux 成立），所以"两台上都接进"不是形式要求。
//
// 这里按真来源对表：`scripts/test-*.sh` 的清单是集合本身，两份脚本引用的名字必须
// 与它相等，两个方向都查（漏接一条红，引用一条不存在的也红）。
func TestEveryDrillIsWiredIntoBothGateTwins(t *testing.T) {
	found, err := filepath.Glob(filepath.Join("..", "..", "scripts", "test-*.sh"))
	if err != nil {
		t.Fatal(err)
	}
	drills := map[string]bool{}
	for _, p := range found {
		drills[filepath.Base(p)] = true
	}
	if len(drills) < 4 {
		t.Fatalf("only %d drill scripts were found in scripts/ - the glob is not seeing them", len(drills))
	}

	twins := map[string]string{
		"bash": filepath.Join("..", "..", "scripts", "ci-local.sh"),
		"pwsh": filepath.Join("..", "..", "scripts", "ci-local.ps1"),
	}
	seen := map[string]map[string]bool{"bash": {}, "pwsh": {}}
	for which, path := range twins {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s (%s): %v", path, which, err)
		}
		for _, m := range regexp.MustCompile(`test-[a-z0-9-]+\.sh`).FindAllString(string(src), -1) {
			seen[which][m] = true
			if !drills[m] {
				t.Errorf("%s references %s, which is not a drill in scripts/", which, m)
			}
		}
	}
	for d := range drills {
		for which := range twins {
			if !seen[which][d] {
				t.Errorf("%s is a drill but %s never runs it (that half of the gate would pass without it)", d, which)
			}
		}
	}
	// 反空转：两份孪生都得真的引用到东西，否则上面那个"两边都有"是拿空集合比空集合。
	for which := range twins {
		if len(seen[which]) == 0 {
			t.Errorf("%s referenced no drill at all - the check above proves nothing", which)
		}
	}
}
