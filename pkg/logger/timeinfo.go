package serverlogger

import (
	"fmt"
	"time"
)

const (
	// DefaultLayout ...
	DefaultLayout = "2006-01-02T15:04:05Z"
	// DatetimeLayout datetime format
	DatetimeLayout = "2006-01-02 15:04:05"
	// DateLayout ...
	DateLayout = "2006-01-02"
	// PeriodTimeDLayout ...
	PeriodTimeDLayout = "2006.01.02"
	// DiffDateTimeLayput ...
	DiffDateTimeLayput = "2006-01-02 15:04"
)

// LocalTime ...
func LocalTime() time.Time {
	loc, _ := time.LoadLocation("Asia/Seoul")
	t := time.Now().In(loc)
	return t
}

// LocalTimeUnix unixtime
func LocalTimeUnix() int64 {
	loc, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		fmt.Println("time LoadLocation", err)
	}
	t := time.Now().In(loc)
	return t.Unix()
}

// GetUnixTimestamp ...
func GetUnixTimestamp(unixTimeStamp int64) string {
	loc, _ := time.LoadLocation("Asia/Seoul")
	timeStamp := time.Unix(unixTimeStamp, 0).In(loc)
	return timeStamp.Format(DatetimeLayout)

}

// GetDateUnixTimestamp ...
func GetDateUnixTimestamp(unixTimeStamp int64) string {
	loc, _ := time.LoadLocation("Asia/Seoul")
	timeStamp := time.Unix(unixTimeStamp, 0).In(loc)
	return timeStamp.Format(DateLayout)
}

// GetDateTimestampFormat ...
func GetDateTimestampFormat(unixTimeStamp int64, format string) string {
	loc, _ := time.LoadLocation("Asia/Seoul")
	timeStamp := time.Unix(unixTimeStamp, 0).In(loc)
	return timeStamp.Format(format)
}

// LocalTimeStr 현재 날짜 시간 리턴 (YYYY-MM-DD hh:mm:ss)
func LocalTimeStr(format string) string {
	loc, _ := time.LoadLocation("Asia/Seoul")
	t := time.Now().In(loc)
	return t.Format(format)
}

// LocalDateStr 현재 날짜 리턴 (YYYY-MM-DD)
func LocalDateStr() string {
	loc, _ := time.LoadLocation("Asia/Seoul")
	t := time.Now().In(loc)
	return t.Format(DateLayout)
}

// LocalStringTimeToTime ...
func LocalStringTimeToTime(timeString string) time.Time {
	loc, _ := time.LoadLocation("Asia/Seoul")
	t, _ := time.ParseInLocation(DatetimeLayout, timeString, loc)

	return t
}

// ToLocalPeriodDayFormat ...
func ToLocalPeriodDayFormat(utcTime string) string {
	time, _ := time.Parse(DefaultLayout, utcTime)
	t := time.Local().Format(PeriodTimeDLayout)
	return t
}

// ToLocalPeriodDayFormatExt ...
func ToLocalPeriodDayFormatExt(utcTime string, inFormat string, outFormat string) string {
	time, _ := time.Parse(inFormat, utcTime)
	t := time.Local().Format(outFormat)
	return t
}

// TimeDiffTimeFormat ...
func TimeDiffTimeFormat(then time.Time) float64 {
	delta := then.Sub(time.Now())
	return delta.Seconds()
}

// TimeDiff ...
func TimeDiff(input string) float64 {
	_, offset := time.Now().Zone()
	diff := time.Duration(offset) * time.Second
	then, err := time.Parse(DiffDateTimeLayput, input)
	if err != nil {
		return 0
	}

	t := then.Add(-diff).In(time.Local)
	delta := t.Sub(time.Now())
	return delta.Seconds()
}

// TimeDiffDay ...
func TimeDiffDay(input string, format string) float64 {
	_, offset := time.Now().Zone()
	diff := time.Duration(offset) * time.Second
	then, err := time.Parse(format, input)
	if err != nil {
		return 0
	}

	t := then.Add(-diff).In(time.Local)
	delta := t.Sub(time.Now())
	return delta.Hours() / 24
}
