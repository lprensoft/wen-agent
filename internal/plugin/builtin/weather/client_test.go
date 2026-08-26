package weather

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
const fcOKDaily = `{"current":{"temperature_2m":18.4,"relative_humidity_2m":82,"apparent_temperature":16.9,"weather_code":61,"wind_speed_10m":9.3},` +
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
