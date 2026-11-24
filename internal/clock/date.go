package clock

import "time"

// dateLayout 是这些函数认的日期写法（与账本分片名、天气的 DayInfo.Date 同一形状）。
const dateLayout = "2006-01-02"

// FirstInstantOfDate 返回 date 在 loc 中实际开始的第一个瞬间。
//
// 有些城市在夏令时切换时直接跳过午夜（如圣地亚哥 2026-09-06 00:00 → 01:00），
// 用 ParseInLocation 解析 00:00 会落到前一天 23:00。这里按 ZoneBounds 把
// 时间轴切成固定偏移的区间，在每个区间里求目标日期的起点，取最早的那个；
// 跨日回拨也不会取到第二个午夜。整日被跳过的日期（如 Samoa 2011-12-30）返回 false。
//
// 「这一天从哪一刻开始」是好几处共用的问题：天气要按当地日期判断某条提醒什么时候
// 过期，用量账本要按当地日期切分窗口。两边各写一份的话，跳过午夜的城市会在其中
// 一处悄悄差一小时（那一小时里的记录落到前一天）。
func FirstInstantOfDate(date string, loc *time.Location) (time.Time, bool) {
	base, err := time.Parse(dateLayout, date) // UTC 的日历日，不带当地 DST
	if err != nil {
		return time.Time{}, false
	}
	// 按 ZoneBounds 把时间轴切成固定偏移的区间，在每个区间里求目标日期的
	// 起点，取最早的那个。不能直接二分：跨日回拨会让当地日期先到达目标日、
	// 又退回前一天、再到达一次（如 America/Goose_Bay 2010-11-07）。
	limit := base.Add(48 * time.Hour)
	var (
		best  time.Time
		found bool
	)
	for t := base.Add(-48 * time.Hour); t.Before(limit); {
		_, end := t.In(loc).ZoneBounds()
		if end.IsZero() || end.After(limit) {
			end = limit
		}
		if !end.After(t) {
			// 闰年末等情况下 ZoneBounds 会返回不前进的边界（等于甚至早于 t）。
			// 不能直接 break——那样会把尚未检查的日期判成不存在。按当前偏移
			// 找到下一次真正的切换，继续核对剩余时段。
			end = nextOffsetChange(t, limit, loc)
		}
		if !end.After(t) {
			break
		}
		_, offset := t.In(loc).Zone()
		dayStart := base.Add(-time.Duration(offset) * time.Second)
		dayEnd := dayStart.Add(24 * time.Hour)
		cand := t
		if cand.Before(dayStart) {
			cand = dayStart
		}
		stop := end
		if stop.After(dayEnd) {
			stop = dayEnd
		}
		if cand.Before(stop) && cand.In(loc).Format(dateLayout) == date && (!found || cand.Before(best)) {
			best, found = cand, true
		}
		t = end
	}
	if !found {
		return time.Time{}, false
	}
	return best.In(loc), true
}

// nextOffsetChange 返回 from 之后 loc 第一次改变 UTC 偏移的时刻；
// 到 limit 都没变时返回 limit。只用于 ZoneBounds 不前进时兜底。
func nextOffsetChange(from, limit time.Time, loc *time.Location) time.Time {
	_, offset := from.In(loc).Zone()
	for prev := from; prev.Before(limit); {
		probe := prev.Add(time.Hour)
		if probe.After(limit) {
			probe = limit
		}
		if _, cur := probe.In(loc).Zone(); cur != offset {
			// 偏移在 (prev, probe] 内变了：二分到第一次变化的秒。
			lo, hi := prev.Unix(), probe.Unix()
			for hi-lo > 1 {
				mid := lo + (hi-lo)/2
				if _, off := time.Unix(mid, 0).In(loc).Zone(); off != offset {
					hi = mid
				} else {
					lo = mid
				}
			}
			return time.Unix(hi, 0)
		}
		prev = probe
	}
	return limit
}
