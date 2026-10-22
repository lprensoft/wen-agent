package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 源码里的翻译调用扫描。
//
// 插件元数据那批键能从声明推出来（插件名 + 配置项的 Key），但另一大批推不出来：
// 提示词片段、工具描述、状态行、会话注记、日志、命令行输出——它们是散在调用点上的
// 一对 (键, 中文)，只能扫源码。漏译不会让程序出错，只会让英文用户面前冒出一行中文，
// 而那有上千处，没人会一条条看过去。
//
// 这也是为什么调用点一律写成 prompt.T(lang, "全写的键", zh) 而不是带前缀的
// p.tr("system", zh)：键全写在调用点上，这个扫描才拿得到，人也才 grep 得到。

// callSite 是一处翻译调用：键、原文，以及它在哪儿（报错时给人看的）。
type callSite struct {
	Key string
	ZH  string
	Pos string
}

// scanned 是一次全仓扫描的结果，按语言包分开。
type scanned struct {
	UI     map[string]callSite // i18n.T / tr / trf —— 给人看的
	Prompt map[string]callSite // prompt.T / prompt.Tf —— 给模型看的
}

// repoRoot 是仓库根目录。测试从 cmd/wen 里跑，往上两级。
const repoRoot = "../.."

// scanRepo 扫出全仓所有键与原文都是常量的翻译调用。
//
// 键或原文不是常量的那些（如 i18n.T(lang, DescKey(st.Name), st.Description)）直接
// 跳过：它们是按声明推导的那一批，由 metadataStrings 覆盖。
func scanRepo(t *testing.T) scanned {
	t.Helper()
	out := scanned{UI: map[string]callSite{}, Prompt: map[string]callSite{}}

	dirs := map[string]bool{}
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// 依赖与生成物不扫，以「.」开头的目录也不扫——与 go 自己认包的规则一致，
			// 其中有 .git，也有 Windows 上放临时构建的 .gotmp（见 CLAUDE.md「常用命令」）：
			// 别的包并行跑测试时会在那里建了又删临时目录，扫到一半就是「找不到文件」。
			// 根目录自己（../..）的名字也以「.」开头，不能被这条规则跳过。
			name := d.Name()
			if name == "vendor" || name == "node_modules" || (path != repoRoot && strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			dirs[filepath.Dir(path)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历源码目录: %v", err)
	}

	for dir := range dirs {
		scanPackage(t, dir, &out)
	}
	return out
}

func scanPackage(t *testing.T, dir string, out *scanned) {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("解析 %s: %v", dir, err)
	}

	for pkgName, pkg := range pkgs {
		// prompt 包内部调自己的 T / Tf 时没有包名前缀，仍然要扫到——
		// prompt.Duration 的十个键就写在那里
		selfPrompt := pkgName == "prompt"
		consts := packageConsts(pkg)
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				kind, keyArg, zhArg := classify(call, selfPrompt)
				if kind == kindNone {
					return true
				}
				key, ok1 := constString(keyArg, consts)
				zh, ok2 := constString(zhArg, consts)
				if !ok1 || !ok2 || key == "" || zh == "" {
					return true
				}
				site := callSite{Key: key, ZH: zh, Pos: fset.Position(call.Pos()).String()}
				if kind == kindPrompt {
					out.Prompt[key] = site
				} else {
					out.UI[key] = site
				}
				return true
			})
		}
	}
}

// 翻译调用的两种去向：给人看的进 i18n 的语言包，给模型看的进 prompt 的语言包。
type scanKind int

const (
	kindNone scanKind = iota
	kindUI
	kindPrompt
)

// classify 认出一处翻译调用，返回它该进哪个集合，以及键与原文那两个实参。
//
// 三种形状：
//
//	prompt.T(lang, key, zh) / prompt.Tf(lang, key, zh, ...)   给模型看的
//	prompt.Text(key, zh)                                      给模型看的（声明在表里的那种）
//	i18n.T(lang, key, zh) / i18n.Tf(...) / i18n.Logf(...)      给人看的
//	tr(key, zh) / trf(...) / logf(...) / fatalf(...)           给人看的（cmd/wen 里的简写）
func classify(call *ast.CallExpr, selfPrompt bool) (kind scanKind, key, zh ast.Expr) {
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		pkg, ok := fn.X.(*ast.Ident)
		if !ok {
			return kindNone, nil, nil
		}
		if pkg.Name == "prompt" && fn.Sel.Name == "Text" && len(call.Args) == 2 {
			return kindPrompt, call.Args[0], call.Args[1]
		}
		if len(call.Args) < 3 || (fn.Sel.Name != "T" && fn.Sel.Name != "Tf" && fn.Sel.Name != "Logf") {
			return kindNone, nil, nil
		}
		switch pkg.Name {
		case "prompt":
			return kindPrompt, call.Args[1], call.Args[2]
		case "i18n":
			return kindUI, call.Args[1], call.Args[2]
		}
	case *ast.Ident:
		switch fn.Name {
		case "tr", "trf", "logf", "fatalf":
			if len(call.Args) >= 2 {
				return kindUI, call.Args[0], call.Args[1]
			}
		}
		if selfPrompt && (fn.Name == "T" || fn.Name == "Tf") && len(call.Args) >= 3 {
			return kindPrompt, call.Args[1], call.Args[2]
		}
	}
	return kindNone, nil, nil
}

// packageConsts 收集包级的字符串常量，供解析写成常量名的键与原文。
func packageConsts(pkg *ast.Package) map[string]string {
	out := map[string]string{}
	// 两遍：常量可能引用同包里定义在后面的另一个常量
	for range 2 {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
					continue
				}
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range vs.Names {
						if i >= len(vs.Values) {
							continue
						}
						if s, ok := constString(vs.Values[i], out); ok {
							out[name.Name] = s
						}
					}
				}
			}
		}
	}
	return out
}

// constString 把一个表达式求成字符串：字面量、包级常量名，以及它们用 + 拼起来的。
// 求不出来返回 false——那说明它是运行时才知道的值，不在这个扫描的职责范围内。
func constString(e ast.Expr, consts map[string]string) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.Ident:
		s, ok := consts[v.Name]
		return s, ok
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, ok1 := constString(v.X, consts)
		r, ok2 := constString(v.Y, consts)
		return l + r, ok1 && ok2
	case *ast.ParenExpr:
		return constString(v.X, consts)
	}
	return "", false
}
