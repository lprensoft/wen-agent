package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"

	"wen/internal/config"
	"wen/internal/i18n"
	"wen/internal/llm"
	"wen/internal/modelcfg"
)

// 首次运行向导。
//
// 新装的人只需要回答一件事：用哪家的哪个模型。其余全部取默认值——端口 8080、
// 只听本机、会话与插件状态落在配置目录、插件按 buildPlugins 的初值开关。端口
// 之所以也问一句，是因为 8080 被别的程序占着时服务根本起不来，而那正是新手最
// 难自己查明白的一种失败；所以向导只在它确实被占用时才把默认值换掉。
//
// 答案的去处各归各的，不新开文件：模型进 models.json（与设置页、wen config 同一份），
// 端口与语言只在和默认值不同、且还没有 config.yaml 时，写进一份只含这几项的
// config.yaml。新用户因此通常一个配置文件都不会有。

// modelsBackend 是向导用得着的那一部分配置出口：在线与离线两种 backend 都满足它。
// 单列一个窄接口，是为了离线时不必把全部插件也装配一遍——向导只碰模型。
type modelsBackend interface {
	loadModels() (modelsDoc, error)
	saveModels(modelcfg.File) error
}

// errOnboardAborted 表示用户中途退出了向导。调用方据此安静地收场，而不是打一行报错。
var errOnboardAborted = errors.New("onboard aborted")

// runOnboard 是 wen onboard：随时重跑一遍首次配置（新增一家提供商并切换过去）。
func runOnboard(args []string) error {
	fs := flag.NewFlagSet("onboard", flag.ExitOnError)
	configPath := fs.String("c", "", tr("cli.flag.config", "配置文件路径（默认 ./config.yaml 或 ~/.wen/config.yaml）"))
	_ = fs.Parse(args)

	if !isInteractive() {
		return errors.New(tr("cli.onboard.needs_tty", "wen onboard 需要在终端里运行（当前的标准输入不是终端）"))
	}
	path := config.ResolvePath(*configPath)
	cfg, err := config.Load(path)
	if err != nil {
		return fmt.Errorf(tr("cli.err.load_config", "加载配置失败: %w"), err)
	}
	adoptLang(cfg)

	var b modelsBackend
	if c, ok := dial(cfg.BaseDir); ok {
		b = &onlineBackend{c: c, addr: c.base}
	} else {
		store, err := modelcfg.NewStore(uiLang, filepath.Join(cfg.BaseDir, "models.json"), cfg)
		if err != nil {
			return fmt.Errorf(tr("cli.err.load_models", "加载模型配置失败: %w"), err)
		}
		b = &offlineBackend{cfg: cfg, models: store}
	}

	err = onboard(b, cfg, path, false)
	if errors.Is(err, errOnboardAborted) {
		fmt.Print(tr("cli.onboard.aborted", "已退出向导，什么都没有改。\n"))
		return nil
	}
	if err != nil {
		return err
	}
	if _, online := b.(*onlineBackend); online {
		fmt.Print(tr("cli.onboard.done_online", "服务正在运行，新模型已经生效。\n"))
	} else {
		fmt.Print(tr("cli.onboard.done_offline", "运行 wen 启动服务。\n"))
	}
	return nil
}

// ensureModel 在服务启动之前确认有一个能用的模型；没有的话，在终端里就进向导，
// 不在终端里（systemd、容器、nohup）就报一句该怎么办。
//
// portFixed 为真表示命令行上用 -p 指定了端口，向导不再问端口。
func ensureModel(cfg *config.Config, path string, portFixed bool) error {
	store, err := modelcfg.NewStore(uiLang, filepath.Join(cfg.BaseDir, "models.json"), cfg)
	if err != nil {
		return fmt.Errorf(tr("cli.err.load_models", "加载模型配置失败: %w"), err)
	}
	_, reason := store.Resolve(uiLang)
	if reason == nil {
		return nil
	}

	// 两种情况不进向导，因为向导解决不了、反而会添乱：
	//
	// 当前提供商写在 config.yaml 里、只是密钥为空——几乎总是引用的环境变量没设。
	// 进向导的话唯一的出路是再加一家提供商，把明文密钥存进 models.json 并切过去，
	// 此后它永久压过 config.yaml 的选择，把环境变量补上也回不来了。
	if p, ok := currentProvider(store.Snapshot()); ok && p.Source == "config" &&
		strings.TrimSpace(p.APIKey) == "" && modelcfg.RequiresKey(p.BaseURL) {
		return errors.New(trf("cli.onboard.config_key_empty",
			"config.yaml 里提供商 %q 的 api_key 为空。它若写成 ${变量名}，请确认这个环境变量在启动 wen 的环境里设置了。", p.Name))
	}
	// 同一配置目录下已有实例在跑：离线改 models.json 会被它下一次保存整个盖掉，
	// 而这一个进程随后也抢不到端口。该走的是 wen onboard，它会经运行中的实例保存。
	if c, ok := dial(cfg.BaseDir); ok {
		return errors.New(trf("cli.onboard.already_running",
			"同一配置目录下已有实例在 %s 运行。要改模型请用 wen onboard 或设置页，改动会当场生效。", c.base))
	}

	if !isInteractive() {
		return errors.New(trf("cli.onboard.headless",
			"还没有可用的模型（%v）。\n在终端里运行一次 wen onboard 完成首次配置即可；"+
				"容器这类没有终端的部署，也可以在 config.yaml 里写好 model 与 providers 两段。", reason))
	}
	fmt.Print(trf("cli.onboard.why", "还没有可用的模型（%v），先花一分钟做个首次配置。\n\n", reason))
	return onboard(&offlineBackend{cfg: cfg, models: store}, cfg, path, portFixed)
}

// currentProvider 取当前选中的提供商（含来源标记）。
func currentProvider(f modelcfg.File) (modelcfg.Provider, bool) {
	for _, p := range f.Providers {
		if strings.EqualFold(p.Name, f.Current.Provider) {
			return p, true
		}
	}
	return modelcfg.Provider{}, false
}

// onboard 走一遍向导。成功时 cfg 里的端口与语言已更新为向导定下的值，
// 调用方可以直接拿它接着启动。
func onboard(b modelsBackend, cfg *config.Config, path string, portFixed bool) error {
	_, online := b.(*onlineBackend)
	_, statErr := os.Stat(path)
	hasConfigFile := statErr == nil
	autoLang := resolveLang(cfg) // 不写 lang: 时服务会用的语言，向导据此判断要不要记下来

	fmt.Print(tr("cli.onboard.head", "Wen Agent 首次配置\n只需要选定模型提供商与模型，其余全部使用默认值，之后可在设置页或 wen config 里修改。\n\n"))

	if err := languageStep(); err != nil {
		return err
	}

	doc, err := b.loadModels()
	if err != nil {
		return err
	}
	if doc.Current.Provider != "" {
		fmt.Print(trf("cli.onboard.has_current", "当前模型是 %s / %s。继续的话会新增一家提供商并切换过去。\n\n",
			doc.Current.Provider, doc.Current.Model))
	}

	// 保存失败（名称重复、地址不合法这类只在保存时把关的规则）就回到选提供商那一步，
	// 而不是带着报错退出——那样前面填的全白填了
	for {
		provider, model, err := providerStep(doc)
		if err != nil {
			return err
		}
		err = saveOnboardModel(b, doc, provider, model)
		if err == nil {
			break
		}
		fmt.Print(trf("cli.onboard.save_failed", "× 保存失败：%v\n\n", err))
	}

	port := 0
	if !portFixed && !online && !hasConfigFile {
		if port, err = portStep(cfg); err != nil {
			return err
		}
	}
	applyStartupAnswers(cfg, path, port, uiLang, autoLang, hasConfigFile, online)
	return nil
}

// saveOnboardModel 把选定的提供商追加进模型配置并切换过去。
func saveOnboardModel(b modelsBackend, doc modelsDoc, provider modelcfg.Provider, model string) error {
	next := doc.toFile()
	next.Providers = append(next.Providers, provider)
	next.Current = modelcfg.Selection{Provider: provider.Name, Model: model}
	if err := b.saveModels(next); err != nil {
		return err
	}
	fmt.Print(trf("cli.onboard.saved_model", "✓ 模型：%s / %s\n", provider.Name, model))
	return nil
}

// applyStartupAnswers 落下端口与语言这两项启动期配置，并改进 cfg 让服务接着用。
// 与提问的部分分开，是为了能脱离终端测它。
//
// 这一步失败不报错：模型已经存好了，为一份 config.yaml 写不进去就让服务起不来
// 不划算。写不进去时把该怎么办说清楚，本次启动仍按选定的值走。
func applyStartupAnswers(cfg *config.Config, path string, port int, lang, autoLang string, hasConfigFile, online bool) {
	// 和默认值一样就什么都不写，新用户因此通常没有 config.yaml
	writePort := 0
	if port != 0 && port != config.DefaultPort {
		writePort = port
	}
	writeLang := ""
	if lang != autoLang && cfg.Lang == "" {
		writeLang = lang
	}
	// 已有 config.yaml（或服务在跑）时一律不写；语言与服务实际会用的不一致就提示一句
	if hasConfigFile || online {
		if effective := resolveLang(cfg); lang != effective {
			fmt.Print(trf("cli.onboard.lang_not_saved",
				"  语言只在这次向导里生效；要让服务也用它，在 %s 里写上 lang: %s\n", path, lang))
		}
		writePort, writeLang = 0, ""
	}

	if writePort != 0 || writeLang != "" {
		header := tr("cli.onboard.yaml_header", "由首次运行向导生成，只记下了与默认值不同的几项。\n其余各项都用默认值，可以照 docs/configuration.md 自行添加。")
		if err := config.CreateMinimal(path, header, writePort, writeLang); err != nil {
			fmt.Print(trf("cli.onboard.yaml_failed", "× 写入 %s 失败：%v\n  选定的端口与语言只在这一次启动生效。\n", path, err))
			if writePort != 0 {
				fmt.Print(trf("cli.onboard.port_hint", "  以后启动请加上 -p %d。\n", writePort))
			}
		} else {
			fmt.Print(trf("cli.onboard.saved_yaml", "✓ 已写入 %s\n", path))
		}
	}
	if port != 0 {
		cfg.Server.Port = port
		fmt.Print(trf("cli.onboard.saved_port", "✓ 端口：%d\n", port))
	}
	if writeLang != "" {
		cfg.Lang = writeLang
	}
	fmt.Print(trf("cli.onboard.config_dir", "  配置目录：%s\n\n", cfg.BaseDir))
}

// languageStep 先定语言：后面每一屏都按它显示，它也是看不懂界面时的逃生口，
// 所以排在第一步（与 wen config 顶层菜单把语言放第一项是同一个理由）。
func languageStep() error {
	choice := uiLang
	opts := make([]huh.Option[string], 0, len(i18n.Languages()))
	for _, l := range i18n.Languages() {
		opts = append(opts, huh.NewOption(l.Endonym, l.Code))
	}
	err := run(huh.NewSelect[string]().
		Title("Language").
		Description(tr("cli.onboard.lang_desc", "界面、发给模型的提示词与运行日志都用这门语言。")).
		Options(opts...).
		Value(&choice))
	if err != nil {
		return aborted(err)
	}
	uiLang = choice
	return nil
}

// providerStep 选提供商、填密钥、选模型，并试连一次。返回要保存的提供商与选中的模型 id。
func providerStep(doc modelsDoc) (modelcfg.Provider, string, error) {
	for {
		p, consoleURL, custom, err := pickPreset(doc)
		if err != nil {
			return modelcfg.Provider{}, "", err
		}
		// 预置自带的模型清单单独留一份：用户自己填的模型 ID 会追加进 p.Models，
		// 「重新填写」时若拿追加后的清单来判断，Ollama 这种不带清单的预置就再也
		// 不出模型 ID 输入框了，填错的名字永远改不掉
		presetModels := append([]modelcfg.Model(nil), p.Models...)
		model := ""
	fill:
		for {
			if custom {
				model, err = customForm(doc, &p)
			} else {
				model, err = presetForm(&p, presetModels, model, consoleURL)
			}
			if err != nil {
				return modelcfg.Provider{}, "", err
			}

			fmt.Print(tr("cli.onboard.probing", "正在测试连接…\n"))
			probeErr := probeProvider(p, model)
			if probeErr == nil {
				fmt.Print(tr("cli.onboard.probe_ok", "✓ 连接成功\n\n"))
				return p, model, nil
			}
			fmt.Print(trf("cli.onboard.probe_failed", "× 连接失败：%v\n\n", probeErr))

			opts := []huh.Option[string]{
				huh.NewOption(tr("cli.onboard.retry", "重新填写"), "retry"),
				huh.NewOption(tr("cli.onboard.other", "换一家提供商"), "other"),
			}
			// 缺密钥时不给「照样保存」：存下来服务也起不来（Resolve 会拒绝），
			// 下次启动又回到向导，还多留下一条坏掉的提供商
			if !errors.Is(probeErr, errMissingKey) {
				opts = append(opts, huh.NewOption(tr("cli.onboard.save_anyway", "不管它，照样保存"), "save"))
			}
			opts = append(opts, huh.NewOption(tr("cli.onboard.quit", "退出向导"), "quit"))
			next := "retry"
			if err := run(huh.NewSelect[string]().
				Title(tr("cli.onboard.after_fail", "怎么办？")).
				Options(opts...).
				Value(&next)); err != nil {
				return modelcfg.Provider{}, "", aborted(err)
			}
			switch next {
			case "save":
				return p, model, nil
			case "other":
				break fill
			case "quit":
				return modelcfg.Provider{}, "", errOnboardAborted
			}
		}
	}
}

// pickPreset 从预置表里挑一家，或选「自定义」。
func pickPreset(doc modelsDoc) (p modelcfg.Provider, consoleURL string, custom bool, err error) {
	opts := make([]huh.Option[string], 0, len(doc.Presets)+1)
	for _, pv := range doc.Presets {
		opts = append(opts, huh.NewOption(fit(pv.Name+" — "+pv.Description), pv.Key))
	}
	opts = append(opts, huh.NewOption(fit(tr("cli.models.presetCustom", "自定义 — 上面没有你要的那家，手工填写")), "custom"))

	choice := opts[0].Value
	if err := run(huh.NewSelect[string]().
		Title(tr("cli.onboard.provider_title", "选择模型提供商")).
		Description(fitLines(tr("cli.models.presetDesc", "挑一家即可，地址与常用模型都已填好，你只要补上 API Key。"))).
		Height(listHeight(len(opts), 3)).
		Options(opts...).
		Value(&choice)); err != nil {
		return p, "", false, aborted(err)
	}
	if choice == "custom" {
		return modelcfg.Provider{Type: llm.TypeOpenAICompat}, "", true, nil
	}
	for _, pv := range doc.Presets {
		if pv.Key == choice {
			return modelcfg.Provider{
				Name: pv.Name, Type: pv.Type, BaseURL: pv.BaseURL, Dialect: pv.Dialect,
				Models: append([]modelcfg.Model(nil), pv.Models...),
			}, pv.ConsoleURL, false, nil
		}
	}
	return p, "", false, errOnboardAborted // 不会走到：选项只来自上面那张表
}

// presetForm 是预置那一支：只问密钥与模型，地址、方言、模型清单都是填好的。
// presetModels 是预置自带的模型清单，last 是上一轮填过的模型（重新填写时预填）。
func presetForm(p *modelcfg.Provider, presetModels []modelcfg.Model, last, consoleURL string) (string, error) {
	var fields []huh.Field
	key := p.APIKey
	if modelcfg.RequiresKey(p.BaseURL) {
		desc := tr("cli.onboard.key_desc", "输入时不回显")
		if consoleURL != "" {
			desc = trf("cli.onboard.key_from", "去这里拿：%s", consoleURL)
		}
		fields = append(fields, huh.NewInput().Title("API Key").Description(desc).
			EchoMode(huh.EchoModePassword).Value(&key).
			Validate(func(s string) error {
				if strings.TrimSpace(s) == "" {
					return errors.New(tr("cli.onboard.key_empty", "API Key 不能为空"))
				}
				return nil
			}))
	}

	model := last
	if model == "" && len(presetModels) > 0 {
		model = presetModels[0].ID
	}
	switch {
	case len(presetModels) == 0:
		// 预置里没带模型清单的（如 Ollama：装了哪些模型因人而异），让用户自己填
		fields = append(fields, modelIDInput(&model))
	case len(presetModels) > 1:
		opts := make([]huh.Option[string], 0, len(presetModels))
		for _, m := range presetModels {
			opts = append(opts, huh.NewOption(m.ID, m.ID))
		}
		fields = append(fields, huh.NewSelect[string]().
			Title(tr("cli.onboard.model_title", "模型")).
			Description(tr("cli.onboard.model_desc", "第一项是推荐的默认选择")).
			Options(opts...).Value(&model))
	}
	if len(fields) > 0 {
		if err := huh.NewForm(huh.NewGroup(fields...)).Run(); err != nil {
			return "", aborted(err)
		}
	}
	p.APIKey = strings.TrimSpace(key)
	model = strings.TrimSpace(model)
	p.Models = append([]modelcfg.Model(nil), presetModels...)
	if _, ok := findModelID(p.Models, model); !ok {
		p.Models = append(p.Models, modelcfg.Model{ID: model})
	}
	return model, nil
}

// customForm 是「自定义」那一支：预置表里没有的服务，地址与模型都要自己填。
func customForm(doc modelsDoc, p *modelcfg.Provider) (string, error) {
	name, typ, baseURL, key, dialect := p.Name, p.Type, p.BaseURL, p.APIKey, p.Dialect
	model := ""
	if len(p.Models) > 0 {
		model = p.Models[0].ID
	}

	typeOpts := make([]huh.Option[string], 0)
	for _, t := range modelcfg.TypeOptions(uiLang) {
		typeOpts = append(typeOpts, huh.NewOption(t["label"], t["value"]))
	}
	dialectOpts := make([]huh.Option[string], 0)
	for _, d := range modelcfg.DialectOptions(uiLang) {
		dialectOpts = append(dialectOpts, huh.NewOption(d["label"], d["value"]))
	}

	err := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title(tr("cli.models.name", "名称")).Description(tr("cli.models.nameDesc", "唯一标识，也是界面上的显示名")).
			Value(&name).Validate(func(s string) error {
			s = strings.TrimSpace(s)
			if s == "" {
				return errors.New(tr("cli.models.nameEmpty", "名称不能为空"))
			}
			for _, existing := range doc.Providers {
				if strings.EqualFold(existing.Name, s) {
					return errors.New(trf("cli.onboard.name_taken", "已经有一个叫 %s 的提供商了", existing.Name))
				}
			}
			return nil
		}),
		huh.NewSelect[string]().Title(tr("cli.models.apiType", "接口类型")).Options(typeOpts...).Value(&typ),
		huh.NewInput().Title("Base URL").Value(&baseURL).Validate(func(s string) error {
			// 与保存时同一条规则：只在保存时才报，前面填的就全白填了
			return modelcfg.CheckBaseURL(uiLang, s)
		}),
		huh.NewInput().Title("API Key").Description(tr("cli.onboard.key_custom_desc", "本机推理服务（localhost）可以留空")).
			EchoMode(huh.EchoModePassword).Value(&key).Validate(func(s string) error {
			if strings.TrimSpace(s) == "" && modelcfg.RequiresKey(baseURL) {
				return errors.New(tr("cli.onboard.key_required", "远端服务需要 API Key，只有本机地址可以留空"))
			}
			return nil
		}),
		modelIDInput(&model),
		huh.NewSelect[string]().Title(tr("cli.models.dialect", "思考参数方言")).
			Description(tr("cli.models.dialectDesc", "仅 openai_compat 生效")).Options(dialectOpts...).Value(&dialect),
	)).Run()
	if err != nil {
		return "", aborted(err)
	}
	model = strings.TrimSpace(model)
	*p = modelcfg.Provider{
		Name: strings.TrimSpace(name), Type: typ,
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:  strings.TrimSpace(key), Dialect: dialect,
		Models: []modelcfg.Model{{ID: model}},
	}
	return model, nil
}

func modelIDInput(model *string) huh.Field {
	return huh.NewInput().Title(tr("cli.onboard.model_id", "模型 ID")).
		Description(tr("cli.onboard.model_id_desc", "传给接口的模型名，照服务商文档填写")).
		Value(model).Validate(func(s string) error {
		if strings.TrimSpace(s) == "" {
			return errors.New(tr("cli.onboard.model_empty", "模型 ID 不能为空"))
		}
		return nil
	})
}

func findModelID(ms []modelcfg.Model, id string) (modelcfg.Model, bool) {
	for _, m := range ms {
		if m.ID == id {
			return m, true
		}
	}
	return modelcfg.Model{}, false
}

// errMissingKey 表示还没填密钥。单列出来，是因为这种失败不能「照样保存」。
var errMissingKey = errors.New("missing api key")

// probeProvider 用填好的参数发一次最小请求，判据与设置页的「测试连接」相同（llm.Probe）。
func probeProvider(p modelcfg.Provider, model string) error {
	if p.APIKey == "" && modelcfg.RequiresKey(p.BaseURL) {
		return fmt.Errorf("%s: %w", tr("cli.onboard.key_empty", "API Key 不能为空"), errMissingKey)
	}
	provider, err := llm.New(llm.Config{
		Type: p.Type, BaseURL: p.BaseURL, APIKey: p.APIKey, Dialect: p.Dialect,
		PromptCache: p.PromptCache == nil || *p.PromptCache,
		Lang:        uiLang,
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return llm.Probe(ctx, uiLang, provider, model)
}

// portStep 问监听端口。默认端口空着就直接用它（回车即过），被占了就预先换成
// 下一个空着的——向导存在的一半理由就是别让新手卡在「端口已被占用」上。
func portStep(cfg *config.Config) (int, error) {
	host := cfg.Server.Host
	want := cfg.Server.Port
	desc := trf("cli.onboard.port_free", "默认 %d，直接回车即可。", want)
	if !portFree(host, want) {
		if alt := nextFreePort(host, want); alt != 0 {
			desc = trf("cli.onboard.port_taken", "%d 已被别的程序占用，已换成 %d。", want, alt)
			want = alt
		}
	}
	text := strconv.Itoa(want)
	err := run(huh.NewInput().
		Title(tr("cli.onboard.port_title", "监听端口")).
		Description(desc).
		Value(&text).
		Validate(func(s string) error {
			n, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil || n < 1 || n > 65535 {
				return errors.New(tr("cli.onboard.port_invalid", "端口需是 1 到 65535 之间的整数"))
			}
			if !portFree(host, n) {
				return errors.New(trf("cli.onboard.port_busy", "%d 已被占用，换一个", n))
			}
			return nil
		}))
	if err != nil {
		return 0, aborted(err)
	}
	n, _ := strconv.Atoi(strings.TrimSpace(text))
	return n, nil
}

// portFree 试着在该地址上监听一下，能监听就说明端口空着。
func portFree(host string, port int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// nextFreePort 从 port+1 往后找一个空着的端口，找一百个都不行就放弃（返回 0）。
func nextFreePort(host string, port int) int {
	for p := port + 1; p <= port+100 && p <= 65535; p++ {
		if portFree(host, p) {
			return p
		}
	}
	return 0
}

// aborted 把 huh 的「用户按了 Ctrl+C」换成向导自己的哨兵错误，其余原样返回。
func aborted(err error) error {
	if errors.Is(err, huh.ErrUserAborted) {
		return errOnboardAborted
	}
	return err
}
