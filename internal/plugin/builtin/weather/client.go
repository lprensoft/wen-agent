package weather

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"wen/internal/prompt"
)

// 数据源是 Open-Meteo 的公开接口：免费、无需密钥、返回 JSON。
//
// 刻意不走 fetch_url 抓网页再剥标签：天气页面改版一次解析就全错，而这里要的是
// 每半小时无人值守取一次的稳定结果。
// 两个接口地址是变量而非常量，只为让测试指向本地的桩服务：真去打 open-meteo 的测试
// 既慢又不可复现。运行时不改它们。
var (
	geocodeURL  = "https://geocoding-api.open-meteo.com/v1/search"
	forecastURL = "https://api.open-meteo.com/v1/forecast"
)

// maxRespBytes 限制单次响应的读取量。对方是可信接口，但读取量仍要有上界。
const maxRespBytes = 256 * 1024

// Place 是一处解析出来的地点。城市名到经纬度的解析结果几乎不变，因此按地名缓存，
// 不必每次取天气都重解析一遍。
type Place struct {
	Name string // 展示用的地名，如「杭州 · 浙江省 · 中国」
	Lat  float64
	Lon  float64
}

// Report 是一次天气观测。
type Report struct {
	Place     string
	Condition string // 中文天气现象（由 WMO 代码翻译）
	TempC     float64
	FeelsC    float64
	Humidity  int
	WindKmh   float64
	// Fetched 是取到这份数据的本地时刻。过期判定用它而不是接口给的观测时间：
	// 我们要防的是「取不到新数据还在拿旧的当现在用」，那是取数这一侧的事。
	Fetched time.Time
	// Yesterday / Today / Tomorrow 是昨天、今天与明天的概要，与现况同一次请求带回
	// （零额外调用）。旧缓存没有这几项，零值表示未知、不注入，下次刷新自动补上。
	Yesterday DayInfo `json:"Yesterday,omitzero"`
	Today     DayInfo `json:"Today,omitzero"`
	Tomorrow  DayInfo `json:"Tomorrow,omitzero"`
}

// DayInfo 是一天的天气概要。Date 是当地日期（如 2026-08-21）：「明天」会在午夜
// 之后变成「今天」，渲染与预报理由的时效都要靠它判断，不能只信字段名。
type DayInfo struct {
	Date      string
	Condition string
	MinC      float64
	MaxC      float64
	// Seen 是这一天的预报第一次**出现在角色眼前**（被注入）的本地时刻，不是系统取到
	// 的时刻——两者在现实里是分开的：凌晨取到的明天预报，角色要到傍晚才会去看。
	// 注入时据此标出「早就知道了」：预报每轮都在眼前，没有这个标记，模型会把几小时
	// 前就看过的「明天有雨」当成新消息，一遍遍提醒带伞。零值表示还没给角色看过。
	Seen time.Time `json:"Seen,omitzero"`
	// Cued 表示这一天的降水预报已经投过一次开口理由。按「同一天、同样有没有降水」
	// 延续：刷新不重投，预报变了（雨来了 / 雨没了）才算新消息。
	Cued bool `json:"Cued,omitempty"`
}

// carryForecast 把上一次观测里明天预报的「给角色看过了 / 理由投过了」延续到新观测：
// 同一天、降水与否也没变，就是同一条消息，Seen 与 Cued 沿用；否则都从头开始。
func carryForecast(prev, cur DayInfo, prevOK bool) DayInfo {
	if prevOK && prev.known() && cur.known() && prev.Date == cur.Date && isWet(prev.Condition) == isWet(cur.Condition) {
		cur.Seen, cur.Cued = prev.Seen, prev.Cued
	}
	return cur
}

// known 报告这一天是否有数据。
func (d DayInfo) known() bool { return d.Condition != "" && d.Date != "" }

// geocode 把地名解析成经纬度。
func geocode(ctx context.Context, lang string, client *http.Client, name string) (Place, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Place{}, fmt.Errorf("%s", prompt.T(lang, "prompt.weather.err.no_city", "没有填写城市"))
	}
	q := url.Values{}
	q.Set("name", name)
	q.Set("count", "1")
	q.Set("language", "zh")
	q.Set("format", "json")

	var body struct {
		Results []struct {
			Name      string  `json:"name"`
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
			Country   string  `json:"country"`
			Admin1    string  `json:"admin1"`
		} `json:"results"`
	}
	if err := getJSON(ctx, client, geocodeURL+"?"+q.Encode(), &body); err != nil {
		return Place{}, fmt.Errorf(prompt.T(lang, "prompt.weather.err.geocode", "解析地址失败: %w"), err)
	}
	if len(body.Results) == 0 {
		return Place{}, fmt.Errorf(prompt.T(lang, "prompt.weather.err.not_found", "找不到 %q 这个地方，换一个写法试试（如「杭州」「Hangzhou」）"), name)
	}
	r := body.Results[0]
	parts := []string{r.Name}
	for _, s := range []string{r.Admin1, r.Country} {
		if s != "" && s != r.Name {
			parts = append(parts, s)
		}
	}
	return Place{Name: strings.Join(parts, " · "), Lat: r.Latitude, Lon: r.Longitude}, nil
}

// fetchCurrent 取一处地点此刻的天气。
func fetchCurrent(ctx context.Context, lang string, client *http.Client, p Place) (Report, error) {
	q := url.Values{}
	q.Set("latitude", strconv.FormatFloat(p.Lat, 'f', 4, 64))
	q.Set("longitude", strconv.FormatFloat(p.Lon, 'f', 4, 64))
	q.Set("current", "temperature_2m,relative_humidity_2m,apparent_temperature,weather_code,wind_speed_10m")
	// 昨天与明天的概要搭同一次请求：past_days=1 + forecast_days=2 使 daily 数组
	// 恰为「昨天、今天、明天」三天，不多打一次接口
	q.Set("daily", "weather_code,temperature_2m_max,temperature_2m_min")
	q.Set("past_days", "1")
	q.Set("forecast_days", "2")
	q.Set("timezone", "auto")

	var body struct {
		Current struct {
			Temperature float64 `json:"temperature_2m"`
			Humidity    float64 `json:"relative_humidity_2m"`
			Apparent    float64 `json:"apparent_temperature"`
			Code        int     `json:"weather_code"`
			Wind        float64 `json:"wind_speed_10m"`
		} `json:"current"`
		Daily struct {
			Time []string  `json:"time"`
			Code []int     `json:"weather_code"`
			Max  []float64 `json:"temperature_2m_max"`
			Min  []float64 `json:"temperature_2m_min"`
		} `json:"daily"`
	}
	if err := getJSON(ctx, client, forecastURL+"?"+q.Encode(), &body); err != nil {
		return Report{}, fmt.Errorf(prompt.T(lang, "prompt.weather.err.fetch", "获取天气失败: %w"), err)
	}
	c := body.Current
	rep := Report{
		Place:     p.Name,
		Condition: conditionOf(c.Code),
		TempC:     c.Temperature,
		FeelsC:    c.Apparent,
		Humidity:  int(c.Humidity + 0.5),
		WindKmh:   c.Wind,
		Fetched:   time.Now(),
	}
	// 形状不符（字段缺失、长度不齐）就整体放弃这两项：宁缺勿错
	d := body.Daily
	if len(d.Time) == 3 && len(d.Code) == 3 && len(d.Max) == 3 && len(d.Min) == 3 {
		rep.Yesterday = DayInfo{Date: d.Time[0], Condition: conditionOf(d.Code[0]), MinC: d.Min[0], MaxC: d.Max[0]}
		rep.Today = DayInfo{Date: d.Time[1], Condition: conditionOf(d.Code[1]), MinC: d.Min[1], MaxC: d.Max[1]}
		rep.Tomorrow = DayInfo{Date: d.Time[2], Condition: conditionOf(d.Code[2]), MinC: d.Min[2], MaxC: d.Max[2]}
	}
	return rep, nil
}

// getJSON 发一次 GET 并解析 JSON 响应。
func getJSON(ctx context.Context, client *http.Client, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "wen-agent/1.0")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("接口返回 HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("响应格式无法解析: %w", err)
	}
	return nil
}

// 天气现象的说法。取日常说话会用的词，不用「毛毛雨」这类没人说的书面气象术语。
//
// 中文是**规范形式**：观测缓存里存下来的就是它，isWet 也按它的用字判断降水，
// 只在展示时才按语言译出来。这样换一次语言，磁盘上已有的观测一个字都不用动。
const (
	condClear         = "晴"
	condMostlyClear   = "大致晴朗"
	condCloudy        = "多云"
	condOvercast      = "阴"
	condFog           = "有雾"
	condRimeFog       = "雾凇"
	condLightRain     = "小雨"
	condDrizzle       = "细雨"
	condDenseDrizzle  = "细密的雨"
	condFreezingRain  = "冻雨"
	condModerateRain  = "中雨"
	condHeavyRain     = "大雨"
	condHeavyFreezing = "大的冻雨"
	condLightSnow     = "小雪"
	condModerateSnow  = "中雪"
	condHeavySnow     = "大雪"
	condSnowGrains    = "米雪"
	condShowers       = "阵雨"
	condHeavyShowers  = "大阵雨"
	condSnowShowers   = "阵雪"
	condHeavySnowShow = "大阵雪"
	condThunder       = "雷阵雨"
	condThunderHail   = "雷阵雨伴冰雹"
	condUnknown       = "天气不明"
)

// conditions 把规范值按语言译出来。声明处是一串 (键, 中文) 字面量，覆盖检查扫得到。
var conditions = prompt.NewEnum(
	prompt.Text("prompt.weather.cond.clear", condClear),
	prompt.Text("prompt.weather.cond.mostly_clear", condMostlyClear),
	prompt.Text("prompt.weather.cond.cloudy", condCloudy),
	prompt.Text("prompt.weather.cond.overcast", condOvercast),
	prompt.Text("prompt.weather.cond.fog", condFog),
	prompt.Text("prompt.weather.cond.rime_fog", condRimeFog),
	prompt.Text("prompt.weather.cond.light_rain", condLightRain),
	prompt.Text("prompt.weather.cond.drizzle", condDrizzle),
	prompt.Text("prompt.weather.cond.dense_drizzle", condDenseDrizzle),
	prompt.Text("prompt.weather.cond.freezing_rain", condFreezingRain),
	prompt.Text("prompt.weather.cond.moderate_rain", condModerateRain),
	prompt.Text("prompt.weather.cond.heavy_rain", condHeavyRain),
	prompt.Text("prompt.weather.cond.heavy_freezing_rain", condHeavyFreezing),
	prompt.Text("prompt.weather.cond.light_snow", condLightSnow),
	prompt.Text("prompt.weather.cond.moderate_snow", condModerateSnow),
	prompt.Text("prompt.weather.cond.heavy_snow", condHeavySnow),
	prompt.Text("prompt.weather.cond.snow_grains", condSnowGrains),
	prompt.Text("prompt.weather.cond.showers", condShowers),
	prompt.Text("prompt.weather.cond.heavy_showers", condHeavyShowers),
	prompt.Text("prompt.weather.cond.snow_showers", condSnowShowers),
	prompt.Text("prompt.weather.cond.heavy_snow_showers", condHeavySnowShow),
	prompt.Text("prompt.weather.cond.thunder", condThunder),
	prompt.Text("prompt.weather.cond.thunder_hail", condThunderHail),
	prompt.Text("prompt.weather.cond.unknown", condUnknown),
)

// wmoText 是 WMO 天气代码到规范值的对照。多个代码可以落到同一个说法上。
var wmoText = map[int]string{
	0: condClear, 1: condMostlyClear, 2: condCloudy, 3: condOvercast,
	45: condFog, 48: condRimeFog,
	51: condLightRain, 53: condDrizzle, 55: condDenseDrizzle,
	56: condFreezingRain, 57: condFreezingRain,
	61: condLightRain, 63: condModerateRain, 65: condHeavyRain,
	66: condFreezingRain, 67: condHeavyFreezing,
	71: condLightSnow, 73: condModerateSnow, 75: condHeavySnow, 77: condSnowGrains,
	80: condShowers, 81: condShowers, 82: condHeavyShowers,
	85: condSnowShowers, 86: condHeavySnowShow,
	95: condThunder, 96: condThunderHail, 99: condThunderHail,
}

// conditionOf 把 WMO 天气代码化成规范值。未收录的代码不编造说法，回落到「天气不明」
// ——注入的内容宁可含糊，也不能是错的。
func conditionOf(code int) string {
	if s, ok := wmoText[code]; ok {
		return s
	}
	return condUnknown
}

// probe 完整走一遍「解析地名 → 取天气」，不经过任何缓存。
// 设置页的测试按钮用它：测的就是这个城市能不能解析、接口通不通。
func probe(ctx context.Context, lang string, client *http.Client, location string) (Place, Report, error) {
	place, err := geocode(ctx, lang, client, location)
	if err != nil {
		return Place{}, Report{}, err
	}
	rep, err := fetchCurrent(ctx, lang, client, place)
	if err != nil {
		return place, Report{}, err
	}
	return place, rep, nil
}
