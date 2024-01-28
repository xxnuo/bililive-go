package douyin

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/hr3lxphr6j/requests"
	"github.com/tidwall/gjson"

	"github.com/hr3lxphr6j/bililive-go/src/live"
	"github.com/hr3lxphr6j/bililive-go/src/live/internal"
	"github.com/hr3lxphr6j/bililive-go/src/pkg/utils"
)

const (
	domain = "live.douyin.com"
	cnName = "抖音"

	randomCookieChars          = "1234567890abcdef"
	roomIdCatcherRegex         = `{\\"webrid\\":\\"([^"]+)\\"}`
	mainInfoLineCatcherRegex   = `self.__pace_f.push\(\[1,\s*"[^:]*:([^<]*,null,\{\\"state\\"[^<]*\])\\n"\]\)`
	commonInfoLineCatcherRegex = `self.__pace_f.push\(\[1,\s*\"(\{.*\})\"\]\)`
)

var roomInfoApiForSprintf = "https://live.douyin.com/webcast/room/web/enter/?aid=6383&app_name=douyin_web&live_id=1&device_platform=web&language=zh-CN&browser_language=zh-CN&browser_platform=Win32&browser_name=Chrome&browser_version=116.0.0.0&web_rid=%s"

func init() {
	live.Register(domain, new(builder))
}

type builder struct{}

func (b *builder) Build(url *url.URL, opt ...live.Option) (live.Live, error) {
	return &Live{
		BaseLive:        internal.NewBaseLive(url, opt...),
		responseCookies: make(map[string]string),
		isUsingLegacy:   false,
	}, nil
}

func createRandomCookie() string {
	return utils.GenRandomString(21, randomCookieChars)
}

func createRandomOdintt() string {
	return utils.GenRandomString(160, randomCookieChars)
}

type Live struct {
	internal.BaseLive
	responseCookies             map[string]string
	LastAvailableStringUrlInfos []live.StreamUrlInfo
	isUsingLegacy               bool
}

func (l *Live) getLiveRoomWebPageResponse() (body string, err error) {
	cookies := l.Options.Cookies.Cookies(l.Url)
	cookieKVs := make(map[string]string)
	cookieKVs["__ac_nonce"] = createRandomCookie()
	for key, value := range l.responseCookies {
		cookieKVs[key] = value
	}
	for _, item := range cookies {
		cookieKVs[item.Name] = item.Value
	}

	// proxy, _ := url.Parse("http://localhost:8888")
	requestSession := requests.NewSession(&http.Client{
		// Transport: &http.Transport{
		// 	Proxy: http.ProxyURL(proxy),
		// },
	})
	req, err := requests.NewRequest(
		http.MethodGet,
		l.Url.String(),
		live.CommonUserAgent,
		requests.Cookies(cookieKVs),
		requests.Headers(map[string]interface{}{
			"Cache-Control": "no-cache",
		}),
	)
	if err != nil {
		return
	}
	cookieWithOdinTt := fmt.Sprintf("odin_tt=%s; %s", createRandomOdintt(), req.Header.Get("Cookie"))
	req.Header.Set("Cookie", cookieWithOdinTt)
	resp, err := requestSession.Do(req)
	if err != nil {
		return
	}
	switch code := resp.StatusCode; code {
	case http.StatusOK:
		for _, cookie := range resp.Cookies() {
			l.responseCookies[cookie.Name] = cookie.Value
		}
	default:
		err = fmt.Errorf("failed to get page, code: %v, %w", code, live.ErrInternalError)
		return
	}
	body, err = resp.Text()
	return
}

func getMainInfoLine(body string) (json *gjson.Result, err error) {
	reg, err := regexp.Compile(mainInfoLineCatcherRegex)
	if err != nil {
		return
	}
	match := reg.FindAllStringSubmatch(body, -1)
	if match == nil {
		err = fmt.Errorf("0 match for mainInfoLineCatcherRegex: %s", mainInfoLineCatcherRegex)
		return
	}
	for _, item := range match {
		if len(item) < 2 {
			// err = fmt.Errorf("len(item) = %d", len(item))
			continue
		}
		mainInfoLine := item[1]

		// 获取房间信息
		mainJson := gjson.Parse(fmt.Sprintf(`"%s"`, mainInfoLine))
		if !mainJson.Exists() {
			// err = fmt.Errorf(errorMessageForErrorf+". Invalid json: %s", stepNumberForLog, mainInfoLine)
			continue
		}

		mainJson = gjson.Parse(mainJson.String()).Get("3")
		if !mainJson.Exists() {
			// err = fmt.Errorf(errorMessageForErrorf+". Main json does not exist: %s", stepNumberForLog, mainInfoLine)
			continue
		}

		if mainJson.Get("state.roomStore.roomInfo.room.status_str").Exists() {
			json = &mainJson
			return
		}
	}
	return nil, fmt.Errorf("MainInfoLine not found")
}

func (l *Live) getRoomInfoFromBody(body string) (info *live.Info, streamUrlInfos []live.StreamUrlInfo, err error) {
	const errorMessageForErrorf = "getRoomInfoFromBody() failed on step %d"
	stepNumberForLog := 1
	mainJson, err := getMainInfoLine(body)
	if err != nil {
		err = fmt.Errorf(errorMessageForErrorf+". %s", stepNumberForLog, err.Error())
		return
	}

	isStreaming := mainJson.Get("state.roomStore.roomInfo.room.status_str").String() == "2"
	info = &live.Info{
		Live:     l,
		HostName: mainJson.Get("state.roomStore.roomInfo.anchor.nickname").String(),
		RoomName: mainJson.Get("state.roomStore.roomInfo.room.title").String(),
		Status:   isStreaming,
	}
	if !isStreaming {
		return
	}
	stepNumberForLog++

	// 获取直播流信息
	streamIdPath := "state.streamStore.streamData.H264_streamData.common.stream"
	streamId := mainJson.Get(streamIdPath).String()
	if streamId == "" {
		err = fmt.Errorf(errorMessageForErrorf+". %s does not exist", stepNumberForLog, streamIdPath)
		return
	}
	stepNumberForLog++

	streamUrlInfos = make([]live.StreamUrlInfo, 0, 4)
	reg2, err := regexp.Compile(commonInfoLineCatcherRegex)
	if err != nil {
		return
	}
	match2 := reg2.FindAllStringSubmatch(body, -1)
	if match2 == nil {
		err = fmt.Errorf(errorMessageForErrorf, stepNumberForLog)
		return
	}
	stepNumberForLog++

	for _, item := range match2 {
		if len(item) < 2 {
			err = fmt.Errorf(errorMessageForErrorf+". len(item) = %d", stepNumberForLog, len(item))
			return
		}
		commonJson := gjson.Parse(gjson.Parse(fmt.Sprintf(`"%s"`, item[1])).String())
		if !commonJson.Exists() {
			err = fmt.Errorf(errorMessageForErrorf+". Not valid json: %s", stepNumberForLog, item[1])
			return
		}
		if !commonJson.Get("common").Exists() {
			continue
		}
		commonStreamId := commonJson.Get("common.stream").String()
		if commonStreamId == "" {
			err = fmt.Errorf(errorMessageForErrorf+". No valid common stream ID: %s", stepNumberForLog, item[1])
			return
		}
		if commonStreamId != streamId {
			continue
		}

		commonJson.Get("data").ForEach(func(key, value gjson.Result) bool {
			flv := value.Get("main.flv").String()
			var Url *url.URL
			Url, err = url.Parse(flv)
			if err != nil {
				return true
			}
			paramsString := value.Get("main.sdk_params").String()
			paramsJson := gjson.Parse(paramsString)
			var description strings.Builder
			paramsJson.ForEach(func(key, value gjson.Result) bool {
				description.WriteString(key.String())
				description.WriteString(": ")
				description.WriteString(value.String())
				description.WriteString("\n")
				return true
			})
			Resolution := 0
			resolution := strings.Split(paramsJson.Get("resolution").String(), "x")
			if len(resolution) == 2 {
				x, err := strconv.Atoi(resolution[0])
				if err != nil {
					return true
				}
				y, err := strconv.Atoi(resolution[1])
				if err != nil {
					return true
				}
				Resolution = x * y
			}
			Vbitrate := int(paramsJson.Get("vbitrate").Int())
			streamUrlInfos = append(streamUrlInfos, live.StreamUrlInfo{
				Name:        key.String(),
				Description: description.String(),
				Url:         Url,
				Resolution:  Resolution,
				Vbitrate:    Vbitrate,
			})
			return true
		})
	}
	sort.Slice(streamUrlInfos, func(i, j int) bool {
		if streamUrlInfos[i].Resolution != streamUrlInfos[j].Resolution {
			return streamUrlInfos[i].Resolution > streamUrlInfos[j].Resolution
		} else {
			return streamUrlInfos[i].Vbitrate > streamUrlInfos[j].Vbitrate
		}
	})
	stepNumberForLog++

	return
}

func (l *Live) GetInfo() (info *live.Info, err error) {
	l.isUsingLegacy = false
	body, err := l.getLiveRoomWebPageResponse()
	if err != nil {
		l.LastAvailableStringUrlInfos = nil
		return
	}

	var streamUrlInfos []live.StreamUrlInfo
	info, streamUrlInfos, err = l.getRoomInfoFromBody(body)
	if err == nil {
		l.LastAvailableStringUrlInfos = streamUrlInfos
		return
	}

	// fallback
	l.isUsingLegacy = true
	return l.legacy_GetInfo(body)
}

func (l *Live) GetStreamUrls() (us []*url.URL, err error) {
	if !l.isUsingLegacy {
		if l.LastAvailableStringUrlInfos != nil {
			us = make([]*url.URL, 0, len(l.LastAvailableStringUrlInfos))
			for _, urlInfo := range l.LastAvailableStringUrlInfos {
				us = append(us, urlInfo.Url)
			}
			return
		}
		return nil, fmt.Errorf("failed douyin GetStreamUrls()")
	} else {
		return l.legacy_GetStreamUrls()
	}
}

func (l *Live) GetPlatformCNName() string {
	return cnName
}

// ================ legacy functions ================

func (l *Live) legacy_getRoomId(body string) (string, error) {
	roomId := utils.Match1(roomIdCatcherRegex, body)
	if roomId == "" {
		return "", fmt.Errorf("failed to get RoomId from page, %w", live.ErrInternalError)
	}
	return roomId, nil
}

func (l *Live) legacy_GetStreamUrls() (us []*url.URL, err error) {
	var body string
	body, err = l.getLiveRoomWebPageResponse()
	if err != nil {
		l.LastAvailableStringUrlInfos = nil
		return
	}
	data, err := l.legacy_getRoomInfo(body)
	if err != nil {
		return nil, err
	}
	var urls []string
	data.Get("data.0.stream_url.flv_pull_url").ForEach(func(key, value gjson.Result) bool {
		urls = append(urls, value.String())
		return true
	})
	streamData := gjson.Parse(data.Get("data.0.stream_url.live_core_sdk_data.pull_data.stream_data").String())
	if streamData.Exists() {
		url := streamData.Get("origin.main.flv")
		if url.Exists() {
			urls = append([]string{url.String()}, urls...)
		}
	}
	return utils.GenUrls(urls...)
}

func (l *Live) legacy_getRoomInfo(body string) (*gjson.Result, error) {
	roomId, err := l.legacy_getRoomId(body)
	if err != nil {
		return nil, err
	}

	cookies := l.Options.Cookies.Cookies(l.Url)
	cookieKVs := make(map[string]string)
	cookieKVs["__ac_nonce"] = createRandomCookie()
	for _, item := range cookies {
		cookieKVs[item.Name] = item.Value
	}
	for key, value := range l.responseCookies {
		cookieKVs[key] = value
	}
	roomInfoApi := fmt.Sprintf(roomInfoApiForSprintf, roomId)
	resp, err := requests.Get(
		roomInfoApi,
		live.CommonUserAgent,
		requests.Cookies(cookieKVs),
		requests.Headers(map[string]interface{}{
			"Cache-Control": "no-cache",
		}))
	if err != nil {
		return nil, err
	}
	switch code := resp.StatusCode; code {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, live.ErrRoomNotExist
	default:
		return nil, fmt.Errorf("failed to get page, code: %v, %w", code, live.ErrInternalError)
	}

	body, err = resp.Text()
	if err != nil {
		return nil, err
	}
	result := gjson.Parse(body)
	return &result, nil
}

func (l *Live) legacy_GetInfo(body string) (info *live.Info, err error) {
	data, err := l.legacy_getRoomInfo(body)
	if err != nil {
		return nil, err
	}
	info = &live.Info{
		Live:     l,
		HostName: data.Get("user.nickname").String(),
		RoomName: data.Get("data.0.title").String(),
		Status:   data.Get("data.0.status").Int() == 2,
	}
	return
}
