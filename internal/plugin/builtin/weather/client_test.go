package weather

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubServer 起一个假的 open-meteo，并把两个接口地址指过去（测试结束还原）。
// body 为空表示该接口返回 500。
func stubServer(t *testing.T, geoBody, fcBody string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := fcBody
		if strings.Contains(r.URL.Path, "search") {
			body = geoBody
		}
		if body == "" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	pointAt(t, srv.URL)
	t.Cleanup(srv.Close)
}

// pointAt 把两个接口地址指向桩服务，测试结束还原。
func pointAt(t *testing.T, base string) {
	t.Helper()
	oldGeo, oldFc := geocodeURL, forecastURL
	geocodeURL, forecastURL = base+"/v1/search", base+"/v1/forecast"
	t.Cleanup(func() { geocodeURL, forecastURL = oldGeo, oldFc })
}

// countingStub 起一个记录各接口命中次数与最后一次查询地名的桩服务。
func countingStub(t *testing.T) (geoHits *int, askedName *string) {
	t.Helper()
	hits, asked := 0, ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "search") {
			hits++
			asked = r.URL.Query().Get("name")
			_, _ = w.Write([]byte(geoOK))
			return
		}
		_, _ = w.Write([]byte(fcOK))
	}))
	pointAt(t, srv.URL)
	t.Cleanup(srv.Close)
	return &hits, &asked
}

const geoOK = `{"results":[{"name":"杭州","latitude":30.2936,"longitude":120.1614,"country":"中国","admin1":"浙江省"}]}`

const fcOK = `{"current":{"temperature_2m":18.4,"relative_humidity_2m":82,"apparent_temperature":16.9,"weather_code":61,"wind_speed_10m":9.3}}`

// fcOKDaily 带昨天/今天/明天三天的概要（past_days=1 + forecast_days=2 的形状）。
const fcOKDaily = `{"utc_offset_seconds":-18000,"timezone":"America/New_York","current":{"temperature_2m":18.4,"relative_humidity_2m":82,"apparent_temperature":16.9,"weather_code":61,"wind_speed_10m":9.3},` +
	`"daily":{"time":["2026-08-19","2026-08-20","2026-08-21"],"weather_code":[3,61,63],` +
	`"temperature_2m_max":[24.2,20.1,17.8],"temperature_2m_min":[16.5,15.3,12.1]}}`

func TestFetchCurrentParsesDays(t *testing.T) {
	stubServer(t, geoOK, fcOKDaily)
	rep, err := fetchCurrent(context.Background(), "", http.DefaultClient, Place{Name: "杭州"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Yesterday.Date != "2026-08-19" || rep.Yesterday.Condition != "阴" {
		t.Errorf("昨天 = %+v", rep.Yesterday)
	}
	if rep.Today.Date != "2026-08-20" || !rep.Today.known() {
		t.Errorf("今天 = %+v", rep.Today)
	}
	if rep.Tomorrow.Date != "2026-08-21" || rep.Tomorrow.Condition != "中雨" || rep.Tomorrow.MinC != 12.1 {
		t.Errorf("明天 = %+v", rep.Tomorrow)
	}
	if !rep.UTCOffsetSet || rep.UTCOffsetSeconds != -18000 {
		t.Errorf("时区偏移 = (%v, %d)，期望记录 -18000", rep.UTCOffsetSet, rep.UTCOffsetSeconds)
	}
	if rep.Timezone != "America/New_York" {
		t.Errorf("时区名 = %q", rep.Timezone)
	}
}

// daily 形状不符（长度不齐、字段缺失）时整体放弃这两项，现况照常。
func TestFetchCurrentBadDailyShape(t *testing.T) {
	bad := `{"current":{"temperature_2m":18.4,"weather_code":61},` +
		`"daily":{"time":["2026-08-19","2026-08-20"],"weather_code":[3],"temperature_2m_max":[24.2],"temperature_2m_min":[16.5]}}`
	stubServer(t, geoOK, bad)
	rep, err := fetchCurrent(context.Background(), "", http.DefaultClient, Place{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Yesterday.known() || rep.Tomorrow.known() {
		t.Errorf("形状不符应整体放弃: %+v %+v", rep.Yesterday, rep.Tomorrow)
	}
	if rep.Condition != "小雨" {
		t.Errorf("现况不该受影响: %q", rep.Condition)
	}
}

func TestGeocodeParsesFirstResult(t *testing.T) {
	stubServer(t, geoOK, fcOK)
	pl, err := geocode(context.Background(), "", http.DefaultClient, "杭州")
	if err != nil {
		t.Fatalf("geocode: %v", err)
	}
	if pl.Name != "杭州 · 浙江省 · 中国" {
		t.Errorf("地名 = %q", pl.Name)
	}
	if pl.Lat != 30.2936 || pl.Lon != 120.1614 {
		t.Errorf("坐标 = %v, %v", pl.Lat, pl.Lon)
	}
}

// 查不到的地名必须报错，不能悄悄回落到某个默认位置——注入一个错的地方比不注入更糟。
func TestGeocodeNoResultIsError(t *testing.T) {
	stubServer(t, `{"generationtime_ms":0.1}`, fcOK)
	if _, err := geocode(context.Background(), "", http.DefaultClient, "并不存在的地方"); err == nil {
		t.Fatal("查不到地名时应当报错")
	}
}

func TestGeocodeEmptyName(t *testing.T) {
	if _, err := geocode(context.Background(), "", http.DefaultClient, "   "); err == nil {
		t.Fatal("空地名应当报错")
	}
}

func TestFetchCurrent(t *testing.T) {
	stubServer(t, geoOK, fcOK)
	rep, err := fetchCurrent(context.Background(), "", http.DefaultClient, Place{Name: "杭州", Lat: 30.29, Lon: 120.16})
	if err != nil {
		t.Fatalf("fetchCurrent: %v", err)
	}
	if rep.Condition != "小雨" {
		t.Errorf("天气现象 = %q", rep.Condition)
	}
	if rep.Humidity != 82 {
		t.Errorf("湿度 = %d", rep.Humidity)
	}
	if rep.Fetched.IsZero() {
		t.Error("采集时刻未记录，过期判定会失效")
	}
}

// 请求必须带上经纬度与 current 字段，否则解析出来全是零值。
func TestFetchCurrentSendsCoordinates(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.RawQuery
		_, _ = w.Write([]byte(fcOK))
	}))
	defer srv.Close()
	old := forecastURL
	forecastURL = srv.URL
	defer func() { forecastURL = old }()

	if _, err := fetchCurrent(context.Background(), "", http.DefaultClient, Place{Lat: 30.2936, Lon: 120.1614}); err != nil {
		t.Fatalf("fetchCurrent: %v", err)
	}
	for _, want := range []string{"latitude=30.2936", "longitude=120.1614", "temperature_2m",
		"past_days=1", "forecast_days=2", "daily="} {
		if !strings.Contains(got, want) {
			t.Errorf("请求缺少 %q，实际 %q", want, got)
		}
	}
}

func TestFetchCurrentHTTPError(t *testing.T) {
	stubServer(t, geoOK, "")
	if _, err := fetchCurrent(context.Background(), "", http.DefaultClient, Place{}); err == nil {
		t.Fatal("接口报错时应当返回错误")
	}
}

func TestConditionOfUnknownCode(t *testing.T) {
	if got := conditionOf(61); got != "小雨" {
		t.Errorf("code 61 = %q", got)
	}
	// 未收录的代码不编造说法：注入的内容宁可含糊，也不能是错的
	if got := conditionOf(4242); got != "天气不明" {
		t.Errorf("未知代码 = %q，应当含糊回落", got)
	}
}

// 偏移为 0（UTC）也是有效信息，不能和「旧缓存没存过偏移」混在一起。
func TestFetchCurrentKeepsZeroOffset(t *testing.T) {
	body := `{"utc_offset_seconds":0,` + strings.TrimPrefix(fcOK, "{")
	stubServer(t, geoOK, body)
	rep, err := fetchCurrent(context.Background(), "", http.DefaultClient, Place{Name: "伦敦"})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.UTCOffsetSet || rep.UTCOffsetSeconds != 0 {
		t.Errorf("偏移 = (%v, %d)，期望记录 0 且标记为已知", rep.UTCOffsetSet, rep.UTCOffsetSeconds)
	}
}

// 日期换算要看 IANA 时区，而不是抓取时的固定偏移。
func TestLocalDateUsesIanaZoneAcrossDst(t *testing.T) {
	if _, err := time.LoadLocation("Europe/London"); err != nil {
		t.Skipf("没有时区数据: %v", err)
	}
	r := Report{Timezone: "Europe/London", UTCOffsetSeconds: 0, UTCOffsetSet: true}
	// 2026-03-29 23:30 UTC 已是 3 月 30 日 00:30 BST。
	now := time.Date(2026, 3, 29, 23, 30, 0, 0, time.UTC)
	if got := r.localDate(now); got != "2026-03-30" {
		t.Errorf("localDate = %q，期望按 BST 得到 2026-03-30", got)
	}
}

// 跨日回拨会让当地日期先到达目标日、退回前一天、再到达一次；
// 这里要取第一次到达的时刻，而不是二分碰到的第二个午夜。
func TestFirstInstantOfDateHandlesCrossMidnightFallback(t *testing.T) {
	goose, err := time.LoadLocation("America/Goose_Bay")
	if err != nil {
		t.Skipf("没有时区数据: %v", err)
	}
	got, ok := firstInstantOfDate("2010-11-07", goose)
	want := time.Date(2010, 11, 7, 3, 0, 0, 0, time.UTC)
	if !ok || !got.Equal(want) {
		t.Fatalf("firstInstantOfDate = %v (%v), %v，期望 %v", got, got.UTC(), ok, want)
	}
}

// 闰年末 ZoneBounds 会返回不前进的边界；不能因此把 12 月 31 日与 1 月 1 日
// 判成不存在，导致前一晚的降雨提醒漏发。
func TestFirstInstantOfDateAcrossLeapYearEnd(t *testing.T) {
	cases := []struct {
		zone, date string
		want       time.Time
	}{
		{"Europe/London", "2028-12-31", time.Date(2028, 12, 31, 0, 0, 0, 0, time.UTC)},
		{"Europe/London", "2029-01-01", time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"America/New_York", "2028-12-31", time.Date(2028, 12, 31, 5, 0, 0, 0, time.UTC)},
		{"America/New_York", "2029-01-01", time.Date(2029, 1, 1, 5, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		loc, err := time.LoadLocation(c.zone)
		if err != nil {
			t.Skipf("没有时区数据: %v", err)
		}
		got, ok := firstInstantOfDate(c.date, loc)
		if !ok || !got.Equal(c.want) {
			t.Errorf("%s %s = %v, %v，期望 %v", c.zone, c.date, got, ok, c.want)
		}
	}
}

// 区间不前进时兜底扫描偏移变化；伦敦 2029-03-25 01:00 UTC 进入 BST。
func TestNextOffsetChangeFindsTransition(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skipf("没有时区数据: %v", err)
	}
	from := time.Date(2029, 3, 25, 0, 0, 0, 0, time.UTC)
	got := nextOffsetChange(from, from.Add(48*time.Hour), london)
	want := time.Date(2029, 3, 25, 1, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("nextOffsetChange = %v，期望 %v", got.UTC(), want)
	}
}
