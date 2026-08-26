package main

import (
	"encoding/json"
	"os"
	"sort"
	"testing"

	"wen/internal/config"
	"wen/internal/i18n"
	"wen/internal/plugin"
)

// 插件元数据的翻译键从插件自己的声明推出来，没有一份手写的清单——那也意味着
// 「哪些键还没翻」只能靠跑一遍才知道。这个测试就是那一遍：它把全部内置插件建
// 起来（不初始化，只读声明），推出全部键，与语言包逐一对照。
//
// 漏译不会让程序出错，只会让英文界面上冒出一行中文，而那种缺陷肉眼极难发现——
// 二十九张卡片、一百多个配置项，谁也不会一条条看过去。所以把它交给测试。
//
// 要生成/补全语言包的骨架：
//
//	WEN_I18N_DUMP=zh go test ./cmd/wen -run TestLocaleCoverage   # 全部键与中文原文
//	WEN_I18N_DUMP=missing go test ./cmd/wen -run TestLocaleCoverage
//
// 后者只列还没翻的，输出可以直接拿去填。

// hasCJK 判断一段文本里有没有中文。用它决定某个默认值是否需要译文：
// api_base 这类默认值是网址与标识符，没有可译的东西；只有本来就写成中文的
// 那些（提示词、台词、部位名）漏译才会让英文用户看到一框中文。
func hasCJK(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

func metadataStrings(t *testing.T) map[string]string {
	t.Helper()
	cfg := &config.Config{BaseDir: t.TempDir()}
	m := buildPlugins(cfg, plugin.InitContext{Workdir: t.TempDir()}, nil, plugin.WithoutInit())
	m.Resolve()

	out := map[string]string{}
	put := func(key, zh string) {
		if zh == "" {
			return // 没有原文就没有要翻的东西（说明是可选字段，插件没写）
		}
		out[key] = zh
	}
	for _, st := range m.List() {
		put(plugin.DescKey(st.Name), st.Description)
		put(plugin.CategoryTextKey(st.CategoryKey), st.Category)
		for _, f := range st.ConfigFields {
			put(plugin.FieldLabelKey(st.Name, f.Key), f.Label)
			put(plugin.FieldDescKey(st.Name, f.Key), f.Description)
			// 文本类字段的默认值也要有译文：它是设置页上预填给用户的内容，
			// 漏了的话英文用户开箱看到一框中文（见 plugin.DefaultKey 的说明）。
			if def, ok := f.Default.(string); ok && hasCJK(def) &&
				(f.Type == plugin.FieldString || f.Type == plugin.FieldText) {
				put(plugin.DefaultKey(st.Name, f.Key), def)
			}
			for _, o := range f.Options {
				put(plugin.OptionKey(st.Name, f.Key, o.Value), o.Label)
			}
		}
	}

	// 操作的文案要单独走一趟：Manager.List 只对已启用且初始化过的插件暴露 Actions，
	// 而这里是 WithoutInit 建起来的。直接问插件本身——Actions() 按契约就是廉价、
	// 随时可调的（与 SystemPrompt 同一条），未初始化时它给出的正是默认那一档文案。
	//
	// 掺在里面的运行时事实（技能目录、版本号、已装的包）已经挪进 ActionDef.Detail，
	// 不进语言包也不该进这份清单。
	for _, pl := range builtinPlugins(cfg, nil) {
		a, ok := pl.(plugin.Actionable)
		if !ok {
			continue
		}
		for _, act := range a.Actions() {
			put(plugin.ActionLabelKey(pl.Name(), act.Key), act.Label)
			put(plugin.ActionDescKey(pl.Name(), act.Key), act.Description)
		}
	}
	return out
}

func TestLocaleCoverage(t *testing.T) {
	if err := i18n.Err(); err != nil {
		t.Fatalf("语言包加载失败: %v", err)
	}
	// 两个来源：插件元数据那批能从声明推出来，其余（日志、状态行、会话注记、
	// 命令行输出）散在调用点上，只能扫源码
	want := metadataStrings(t)
	for k, site := range scanRepo(t).UI {
		want[k] = site.ZH
	}

	switch os.Getenv("WEN_I18N_DUMP") {
	case "zh":
		dump(t, want)
		return
	case "missing":
		have := keySet(i18n.Keys(i18n.LangEN))
		missing := map[string]string{}
		for k, zh := range want {
			if !have[k] {
				missing[k] = zh
			}
		}
		dump(t, missing)
		return
	}

	have := keySet(i18n.Keys(i18n.LangEN))
	var missing []string
	for k := range want {
		if !have[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("语言包 en 缺 %d 条译文（用 WEN_I18N_DUMP=missing 导出待填的骨架）:\n%s",
			len(missing), joinLimit(missing, 20))
	}

	// 反向也要查：插件改了配置项的 Key、删了一个字段、调用点换了个键，语言包里
	// 那条就成了死键。死键不会造成故障，但会让「还差多少条」这个数字永远对不上，
	// 而那个数字正是这项工作唯一的进度指示。
	//
	// 这一条查的是**全部**键，不只 plugin.* 的：既然推导与扫描合起来能说清每一条
	// 键的来历，语言包里就不该有第三种来历的东西。
	var stale []string
	for k := range have {
		if _, ok := want[k]; !ok && !derivedElsewhere[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("语言包 en 有 %d 条对不上任何插件声明的死键:\n%s", len(stale), joinLimit(stale, 20))
	}
}

// 这几条 plugin.* 的键不是从插件声明推出来的，因此不算死键：
//   - 「未启用」那句提示由翻译层缀在候选项后面，与具体插件无关；
//   - 「其他」分组只在插件没声明分组时出现，内置插件都声明了，外部插件才会走到；
//   - 「外源插件」分组与 plugin.external.* 那一族只有装进来的插件包才会用到，
//     而它们不在内置清单里——外源插件的名字要装了才知道，那些字段与操作因此
//     共用一个与包名无关的名字（见 plugin.ExternalKeyName）。
var derivedElsewhere = map[string]bool{
	plugin.UnavailableKey:                              true,
	plugin.CategoryTextKey(plugin.CategoryKeyOther):    true,
	plugin.CategoryTextKey(plugin.CategoryKeyExternal): true,
	plugin.FieldLabelKey(ext, "confirm"):               true,
	plugin.FieldDescKey(ext, "confirm"):                true,
	plugin.FieldLabelKey(ext, "max_result_bytes"):      true,
	plugin.FieldDescKey(ext, "max_result_bytes"):       true,
	plugin.ActionLabelKey(ext, "refresh"):              true,
	plugin.ActionDescKey(ext, "refresh"):               true,
}

// ext 是外源插件在语言包里共用的名字，见 plugin.ExternalKeyName。
const ext = plugin.ExternalKeyName

func keySet(keys []string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

func dump(t *testing.T, m map[string]string) {
	t.Helper()
	dumpTo(t, m, "WEN_I18N_OUT")
}

func dumpTo(t *testing.T, m map[string]string, outEnv string) {
	t.Helper()
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// 键多起来之后日志读不了，给个直接落文件的出口
	if out := os.Getenv(outEnv); out != "" {
		if err := os.WriteFile(out, b, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("%d 条已写入 %s", len(m), out)
		return
	}
	t.Logf("%d 条:\n%s", len(m), b)
}

func joinLimit(ss []string, n int) string {
	out := ""
	for i, s := range ss {
		if i == n {
			out += "  …\n"
			break
		}
		out += "  " + s + "\n"
	}
	return out
}
