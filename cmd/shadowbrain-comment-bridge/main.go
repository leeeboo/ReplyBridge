// shadowbrain-comment-bridge connects creator comments to a Feishu approval
// inbox.  It deliberately keeps credentials out of command arguments and
// never prints comment bodies or external API errors.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"comment-bridge/internal/bili"
	"comment-bridge/internal/codexreview"
	"comment-bridge/internal/commentbridge"
	"comment-bridge/internal/envfile"
	"comment-bridge/internal/feishu"
	"comment-bridge/internal/runlock"
	"comment-bridge/internal/secretfile"
	"comment-bridge/internal/youtube"
)

const (
	version                 = "0.1.20"
	defaultDB               = "comment-bridge.db"
	biliRiskAcknowledgement = "I_UNDERSTAND_BILIBILI_WEB_API_ACCOUNT_RISK"
	maxSecretBytes          = 1 << 20
	quietPollInterval       = 20 * time.Minute
	quietStartMinute        = 21 * 60
	quietEndMinute          = 3*60 + 30
)

var japanTimeZone = time.FixedZone("Asia/Tokyo", 9*60*60)

type runConfig struct {
	dbPath       string
	pollInterval time.Duration
	workInterval time.Duration
	feishu       feishu.Config
	youtube      youtubeConfig
	bilibili     biliConfig
	danmakuQueue string
}

type youtubeConfig struct {
	enabled    bool
	channelID  string
	accountID  string
	clientFile string
	tokenFile  string
	maxPages   int
	webhookURL string
}

type biliConfig struct {
	enabled        bool
	credentialFile string
	accountID      string
	subReplyPages  int
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "错误："+err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		printUsage(stdout)
		return nil
	}
	if stdout == nil {
		return errors.New("输出未配置")
	}
	switch args[0] {
	case "run":
		return runBridge(ctx, args[1:])
	case "review-danmaku":
		return runDanmakuWorker(ctx, args[1:])
	case "auth":
		return runAuth(ctx, args[1:], stdout)
	case "discover-feishu":
		return runDiscoverFeishu(ctx, args[1:], stdout)
	case "discover-youtube":
		return runDiscoverYouTube(ctx, args[1:], stdout)
	case "status":
		return runStatus(ctx, args[1:], stdout)
	case "retry-unknown":
		return runRetryUnknown(ctx, args[1:], stdout)
	case "version", "--version", "-version":
		fmt.Fprintln(stdout, "shadowbrain-comment-bridge "+version)
		return nil
	case "help", "--help", "-h":
		printUsage(stdout)
		return nil
	default:
		return errors.New("未知命令")
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `shadowbrain-comment-bridge：将创作者评论送入飞书审核并回帖

用法：
  shadowbrain-comment-bridge run [--db PATH]
  shadowbrain-comment-bridge review-danmaku --queue-dir PATH --codex-bin PATH
  shadowbrain-comment-bridge auth youtube --client-file PATH --token-file PATH
  shadowbrain-comment-bridge discover-feishu --app-id ID --secret-file PATH
  shadowbrain-comment-bridge discover-youtube --client-file PATH --token-file PATH
  shadowbrain-comment-bridge status [--db PATH]
  shadowbrain-comment-bridge retry-unknown [--db PATH] --action ID --confirm-possible-duplicate
  shadowbrain-comment-bridge help | version
`)
}

func runDiscoverYouTube(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("discover-youtube", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	clientFile := fs.String("client-file", "", "OAuth 客户端 JSON")
	tokenFile := fs.String("token-file", "", "OAuth 令牌文件")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || strings.TrimSpace(*clientFile) == "" || strings.TrimSpace(*tokenFile) == "" {
		return errors.New("discover-youtube 参数无效")
	}
	clientData, err := load0600Secret(strings.TrimSpace(*clientFile))
	if err != nil {
		return errors.New("YouTube 客户端文件无效")
	}
	defer clearBytes(clientData)
	credentials, err := youtube.ParseDesktopClientCredentials(clientData)
	if err != nil {
		return errors.New("YouTube 客户端配置无效")
	}
	token, err := youtube.LoadToken(strings.TrimSpace(*tokenFile))
	if err != nil {
		return errors.New("YouTube 令牌文件无效")
	}
	channels, err := youtube.DiscoverOwnedChannels(ctx, youtube.Config{
		ClientID: credentials.ClientID, ClientSecret: credentials.ClientSecret,
		RefreshToken: token.RefreshToken, Scope: token.Scope,
	})
	if err != nil {
		return errors.New("无法读取 YouTube 授权频道")
	}
	if len(channels) == 0 {
		return errors.New("YouTube 授权账号没有可管理频道")
	}
	for _, channel := range channels {
		fmt.Fprintf(stdout, "channel_id=%s title=%q\n", channel.ID, channel.Title)
	}
	return nil
}

func runDiscoverFeishu(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("discover-feishu", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	appID := fs.String("app-id", "", "飞书 App ID")
	secretFile := fs.String("secret-file", "", "飞书 App Secret 文件")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || strings.TrimSpace(*appID) == "" || strings.TrimSpace(*secretFile) == "" {
		return errors.New("discover-feishu 参数无效")
	}
	secret, err := load0600Secret(strings.TrimSpace(*secretFile))
	if err != nil {
		return errors.New("飞书密钥文件无效")
	}
	appSecret := strings.TrimSpace(string(secret))
	clearBytes(secret)
	if appSecret == "" {
		return errors.New("飞书密钥文件无效")
	}
	discoveryCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	fmt.Fprintln(stdout, "waiting_for_group_message=true")
	var once sync.Once
	err = feishu.DiscoverGroupIdentity(discoveryCtx, strings.TrimSpace(*appID), appSecret, func(identity feishu.DiscoveredIdentity) {
		once.Do(func() {
			fmt.Fprintf(stdout, "chat_id=%s open_id=%s\n", identity.ChatID, identity.OpenID)
			cancel()
		})
	})
	if err != nil && ctx.Err() == nil {
		return errors.New("飞书长连接发现失败")
	}
	return nil
}

func runBridge(ctx context.Context, args []string) (runErr error) {
	if err := loadConfiguredEnv(); err != nil {
		return errors.New("加载环境配置失败")
	}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dbOverride := fs.String("db", "", "SQLite 数据库路径")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New("run 参数无效")
	}
	cfg, err := parseRunConfig(os.LookupEnv)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*dbOverride) != "" {
		cfg.dbPath = strings.TrimSpace(*dbOverride)
	}
	lock, err := runlock.Acquire(cfg.dbPath)
	if err != nil {
		return errors.New("无法获取评论桥接数据库运行锁")
	}
	defer lock.Close()
	store, err := commentbridge.Open(cfg.dbPath)
	if err != nil {
		return errors.New("无法打开评论桥接数据库")
	}
	defer store.Close()

	bot, err := feishu.New(cfg.feishu)
	if err != nil {
		return errors.New("飞书机器人配置无效")
	}
	alerts, err := commentbridge.NewErrorAlerts(store, bot.SendErrorAlert)
	if err != nil {
		return errors.New("无法初始化错误告警")
	}
	bot.OnEventError = func(ctx context.Context, err error) { alerts.Observe(ctx, "event", err) }
	defer func() {
		if runErr != nil && ctx.Err() == nil {
			alertCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			alerts.Observe(alertCtx, "startup", runErr)
			_ = alerts.Flush(alertCtx)
		}
	}()
	bridge := &commentbridge.Bridge{
		ErrorAlerts:             alerts,
		Store:                   store,
		Notifier:                bot,
		CredentialAlertNotifier: bot,
		Reactor:                 bot,
		MessageReplier:          bot,
		Repliers:                map[commentbridge.Platform]commentbridge.CommentReplier{},
		Engagers:                map[commentbridge.Platform]commentbridge.CommentEngager{},
		Moderators:              map[commentbridge.Platform]commentbridge.CommentModerator{},
		Refreshers:              map[commentbridge.Platform]commentbridge.CredentialRefresher{},
		QRNotifier:              bot,
		TargetChatID:            cfg.feishu.ChatID,
		YouTubeTargetChatID:     cfg.feishu.YouTubeChatID,
	}
	var danmaku *commentbridge.DanmakuMonitor
	if cfg.youtube.enabled {
		if cfg.youtube.webhookURL != "" {
			bridge.YouTubeWebhook, err = feishu.NewWebhookNotifier(cfg.youtube.webhookURL)
			if err != nil {
				return errors.New("YouTube 飞书 Webhook 配置无效")
			}
		}
		client, e := makeYouTubeClient(cfg.youtube)
		if e != nil {
			return e
		}
		if e := client.ValidateChannel(ctx); e != nil {
			return errors.New("YouTube 授权账号与目标频道不匹配或无法验证")
		}
		bridge.Sources = append(bridge.Sources, client)
		bridge.Repliers[client.Platform()] = client
		bridge.Moderators[client.Platform()] = client
	}
	if cfg.bilibili.enabled {
		adapter, e := makeBiliAdapter(cfg.bilibili, store, bot.SendErrorAlert)
		if e != nil {
			return e
		}
		bridge.Sources = append(bridge.Sources, adapter)
		bridge.Repliers[adapter.Platform()] = adapter
		bridge.Engagers[adapter.Platform()] = adapter
		bridge.Moderators[adapter.Platform()] = adapter
		bridge.Refreshers[adapter.Platform()] = adapter
		adapter.OnSkippedThread = func() { alerts.Observe(ctx, "bilibili_thread", errors.New("thread unavailable")) }
		if cfg.danmakuQueue != "" {
			queue, err := codexreview.NewQueue(cfg.danmakuQueue)
			if err != nil {
				return errors.New("弹幕分析队列目录无效")
			}
			danmaku, err = commentbridge.NewDanmakuMonitor(store, adapter.AccountID(), adapter.ListRecentDanmaku, queue, bot.NotifyDanmaku)
			if err != nil {
				return errors.New("无法初始化弹幕监测")
			}
		}
	}
	if len(bridge.Sources) == 0 {
		return errors.New("至少必须启用一个评论平台")
	}

	// Finish the initial read before opening the approval inbox. This avoids a
	// human reply arriving while source setup/baseline is still incomplete.
	// A failed source is retried by the regular timer; other sources still
	// record their baseline because Bridge.PollOnce aggregates errors.
	if err := bridge.PollOnce(ctx); err != nil {
		logSafe("comment bridge poll failed")
		alerts.Observe(ctx, "poll", err)
	}
	alerts.Observe(ctx, "startup", nil)
	var danmakuTick <-chan time.Time
	if danmaku != nil {
		alerts.Observe(ctx, "danmaku_poll", danmaku.PollOnce(ctx))
		ticker := time.NewTicker(commentbridge.DanmakuInterval)
		defer ticker.Stop()
		danmakuTick = ticker.C
	}

	// Run is isolated by the Feishu wrapper. Its failure is always handled as a
	// safe category: SDK errors may include responses or request details.
	feishuErr := make(chan error, 1)
	go func() { feishuErr <- bot.Run(ctx, bridge) }()
	pollTimer := time.NewTimer(nextCommentPollDelay(time.Now(), cfg.pollInterval))
	defer pollTimer.Stop()
	workTicker := time.NewTicker(cfg.workInterval)
	defer workTicker.Stop()
	alertTicker := time.NewTicker(30 * time.Second)
	defer alertTicker.Stop()
	runWork := func(key string, work func(context.Context) error) {
		err := work(ctx)
		alerts.Observe(ctx, key, err)
		if err != nil {
			logSafe("comment bridge " + key + " failed")
		}
	}
	for {
		select {
		case err := <-feishuErr:
			if err != nil && ctx.Err() == nil {
				alerts.Observe(ctx, "connection", err)
				return errors.New("飞书长连接启动或运行失败")
			}
			return nil
		case <-ctx.Done():
			return nil
		case <-pollTimer.C:
			runWork("poll", bridge.PollOnce)
			pollTimer.Reset(nextCommentPollDelay(time.Now(), cfg.pollInterval))
		case <-danmakuTick:
			runWork("danmaku_poll", danmaku.PollOnce)
		case <-workTicker.C:
			runWork("credential_alert", bridge.CredentialAlertOnce)
			runWork("credential_refresh", bridge.CredentialRefreshOnce)
			runWork("reaction", bridge.ReactionOnce)
			runWork("engagement_feedback", bridge.EngagementFeedbackOnce)
			if danmaku != nil {
				runWork("danmaku", danmaku.WorkOnce)
			}
			runWork("notification", bridge.NotifyOnce)
			runWork("reply", bridge.ReplyOnce)
			runWork("bot_command", bridge.BotCommandOnce)
			runWork("moderation", bridge.ModerationOnce)
			runWork("engagement", bridge.EngagementOnce)
			runWork("manual_engagement_feedback", bridge.ManualEngagementFeedbackOnce)
			runWork("reaction", bridge.ReactionOnce)
			runWork("engagement_feedback", bridge.EngagementFeedbackOnce)
		case <-alertTicker.C:
			if err := alerts.Flush(ctx); err != nil {
				logSafe("comment bridge error alert delivery failed")
			}
		}
	}
}

// nextCommentPollDelay keeps normal polling outside Japan's quiet window and
// reduces it to once every 20 minutes from 21:00 through 03:29:59 JST. The
// delay is capped at either boundary so the new rate takes effect exactly when
// the window changes instead of waiting for an old timer to expire.
func nextCommentPollDelay(now time.Time, normal time.Duration) time.Duration {
	local := now.In(japanTimeZone)
	minute := local.Hour()*60 + local.Minute()
	quiet := minute >= quietStartMinute || minute < quietEndMinute

	if quiet {
		end := time.Date(local.Year(), local.Month(), local.Day(), 3, 30, 0, 0, japanTimeZone)
		if minute >= quietStartMinute {
			end = end.AddDate(0, 0, 1)
		}
		if remaining := end.Sub(local); remaining < quietPollInterval {
			return remaining
		}
		return quietPollInterval
	}

	start := time.Date(local.Year(), local.Month(), local.Day(), 21, 0, 0, 0, japanTimeZone)
	if remaining := start.Sub(local); remaining < normal {
		return remaining
	}
	return normal
}

func logSafe(category string) { fmt.Fprintln(os.Stderr, category) }

func loadConfiguredEnv() error {
	path := strings.TrimSpace(os.Getenv("SHADOWBRAIN_ENV_FILE"))
	if path == "" {
		path = ".env.local"
	}
	return envfile.LoadIfExists(path)
}

func parseRunConfig(lookup func(string) (string, bool)) (runConfig, error) {
	raw := func(key string) string { v, _ := lookup(key); return v }
	value := func(key string) string { return strings.TrimSpace(raw(key)) }
	poll, err := parseDuration(raw("SHADOWBRAIN_COMMENT_POLL_INTERVAL"), 2*time.Minute, 15*time.Second)
	if err != nil {
		return runConfig{}, errors.New("轮询间隔配置无效")
	}
	work, err := parseDuration(raw("SHADOWBRAIN_COMMENT_WORK_INTERVAL"), time.Second, 100*time.Millisecond)
	if err != nil {
		return runConfig{}, errors.New("工作间隔配置无效")
	}
	youtubeEnabled, err := parseBool(raw("SHADOWBRAIN_YOUTUBE_ENABLED"), false)
	if err != nil {
		return runConfig{}, errors.New("YouTube 开关配置无效")
	}
	biliEnabled, err := parseBool(raw("SHADOWBRAIN_BILIBILI_EXPERIMENTAL_ENABLED"), false)
	if err != nil {
		return runConfig{}, errors.New("Bilibili 实验开关配置无效")
	}
	if biliEnabled && raw("SHADOWBRAIN_BILIBILI_EXPERIMENTAL_RISK_ACK") != biliRiskAcknowledgement {
		return runConfig{}, errors.New("Bilibili 实验功能需要精确风险确认")
	}
	maxPages, err := parseBoundedInt(raw("SHADOWBRAIN_YOUTUBE_MAX_PAGES"), 2, 1, 20)
	if err != nil {
		return runConfig{}, errors.New("YouTube MAX_PAGES 配置无效")
	}
	biliSubReplyPages, err := parseBoundedInt(raw("SHADOWBRAIN_BILIBILI_MAX_SUB_REPLY_PAGES"), 10, 1, 50)
	if err != nil {
		return runConfig{}, errors.New("Bilibili 子回复页数配置无效")
	}
	allowAllGroupMembers, err := parseBool(raw("SHADOWBRAIN_FEISHU_ALLOW_ALL_GROUP_MEMBERS"), false)
	if err != nil {
		return runConfig{}, errors.New("飞书群全员授权开关配置无效")
	}
	allowed := splitNonEmpty(value("SHADOWBRAIN_FEISHU_ALLOWED_OPEN_IDS"))
	cfg := runConfig{
		dbPath:       defaultString(value("SHADOWBRAIN_COMMENT_BRIDGE_DB"), defaultDB),
		pollInterval: poll,
		workInterval: work,
		feishu: feishu.Config{
			AppID: value("SHADOWBRAIN_FEISHU_APP_ID"), AppSecret: "", ChatID: value("SHADOWBRAIN_FEISHU_CHAT_ID"), YouTubeChatID: value("SHADOWBRAIN_YOUTUBE_FEISHU_CHAT_ID"), AllowedOpenIDs: allowed, AllowAllGroupMembers: allowAllGroupMembers,
		},
		youtube:      youtubeConfig{enabled: youtubeEnabled, channelID: value("SHADOWBRAIN_YOUTUBE_CHANNEL_ID"), accountID: value("SHADOWBRAIN_YOUTUBE_ACCOUNT_ID"), clientFile: value("SHADOWBRAIN_YOUTUBE_CLIENT_FILE"), tokenFile: value("SHADOWBRAIN_YOUTUBE_TOKEN_FILE"), maxPages: maxPages},
		bilibili:     biliConfig{enabled: biliEnabled, credentialFile: value("SHADOWBRAIN_BILIBILI_CREDENTIAL_FILE"), accountID: value("SHADOWBRAIN_BILIBILI_ACCOUNT_ID"), subReplyPages: biliSubReplyPages},
		danmakuQueue: value("SHADOWBRAIN_DANMAKU_QUEUE_DIR"),
	}
	if cfg.danmakuQueue != "" && !biliEnabled {
		return runConfig{}, errors.New("弹幕监测需要启用 Bilibili")
	}
	secretPath := value("SHADOWBRAIN_FEISHU_APP_SECRET_FILE")
	if cfg.feishu.AppID == "" || secretPath == "" || cfg.feishu.ChatID == "" {
		return runConfig{}, errors.New("飞书配置不完整")
	}
	if cfg.feishu.AllowAllGroupMembers && len(cfg.feishu.AllowedOpenIDs) != 0 {
		return runConfig{}, errors.New("飞书授权模式配置冲突")
	}
	if !cfg.feishu.AllowAllGroupMembers && len(cfg.feishu.AllowedOpenIDs) == 0 {
		return runConfig{}, errors.New("飞书授权模式配置不完整")
	}
	secret, err := load0600Secret(secretPath)
	if err != nil {
		return runConfig{}, errors.New("飞书密钥文件无效")
	}
	cfg.feishu.AppSecret = strings.TrimSpace(string(secret))
	clearBytes(secret)
	if cfg.feishu.AppSecret == "" {
		return runConfig{}, errors.New("飞书密钥文件无效")
	}
	if youtubeEnabled && (cfg.youtube.channelID == "" || cfg.youtube.clientFile == "" || cfg.youtube.tokenFile == "") {
		return runConfig{}, errors.New("YouTube 配置不完整")
	}
	if path := value("SHADOWBRAIN_YOUTUBE_FEISHU_WEBHOOK_FILE"); path != "" && youtubeEnabled {
		secret, err := load0600Secret(path)
		if err != nil {
			return runConfig{}, errors.New("YouTube 飞书 Webhook 文件无效")
		}
		cfg.youtube.webhookURL = strings.TrimSpace(string(secret))
		clearBytes(secret)
		if cfg.youtube.webhookURL == "" {
			return runConfig{}, errors.New("YouTube 飞书 Webhook 文件为空")
		}
	}
	if cfg.feishu.YouTubeChatID != "" && (!youtubeEnabled || cfg.youtube.webhookURL != "") {
		return runConfig{}, errors.New("YouTube 独立回复群需要启用 YouTube，且不能同时配置 Webhook")
	}
	if !youtubeEnabled && !biliEnabled {
		return runConfig{}, errors.New("至少必须启用一个评论平台")
	}
	return cfg, nil
}

func parseBool(raw string, fallback bool) (bool, error) {
	if raw == "" {
		return fallback, nil
	}
	value, err := strconvParseBool(raw)
	if err != nil {
		return false, err
	}
	return value, nil
}

// Kept as a variable-free wrapper so config parsing remains easy to exercise.
func strconvParseBool(raw string) (bool, error) { return strconv.ParseBool(raw) }

func parseDuration(raw string, fallback, minimum time.Duration) (time.Duration, error) {
	if raw == "" {
		return fallback, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil || v < minimum {
		return 0, errors.New("invalid duration")
	}
	return v, nil
}

func parseBoundedInt(raw string, fallback, min, max int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < min || v > max {
		return 0, errors.New("invalid integer")
	}
	return v, nil
}

func splitNonEmpty(raw string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		out = append(out, part)
	}
	return out
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func load0600Secret(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		return nil, errors.New("invalid secret file")
	}
	data, err := secretfile.Load(path, maxSecretBytes)
	if err != nil {
		return nil, err
	}
	// secretfile guarantees a stable regular private file while reading. Check
	// the stronger command-level 0600 policy once more before using its bytes.
	info, err = os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		return nil, errors.New("invalid secret file")
	}
	return data, nil
}

func clearBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

func makeYouTubeClient(cfg youtubeConfig) (*youtube.Client, error) {
	clientData, err := load0600Secret(cfg.clientFile)
	if err != nil {
		return nil, errors.New("YouTube 客户端文件无效")
	}
	defer clearBytes(clientData)
	credentials, err := youtube.ParseDesktopClientCredentials(clientData)
	if err != nil {
		return nil, errors.New("YouTube 客户端配置无效")
	}
	token, err := youtube.LoadToken(cfg.tokenFile)
	if err != nil {
		return nil, errors.New("YouTube 令牌文件无效")
	}
	client, err := youtube.NewClient(youtube.Config{ClientID: credentials.ClientID, ClientSecret: credentials.ClientSecret, RefreshToken: token.RefreshToken, ChannelID: cfg.channelID, AccountID: cfg.accountID, Scope: token.Scope, MaxPages: cfg.maxPages})
	if err != nil {
		return nil, errors.New("YouTube 授权配置无效")
	}
	return client, nil
}

func makeBiliAdapter(cfg biliConfig, store *commentbridge.Store, send func(context.Context, string, string) (string, error)) (*bili.CommentAdapter, error) {
	path := cfg.credentialFile
	if path == "" {
		var err error
		path, err = bili.DefaultCredentialPath()
		if err != nil {
			return nil, errors.New("Bilibili 凭据路径无效")
		}
	}
	credential, err := bili.LoadCredentialFile(path)
	if err != nil || credential.Cookies["SESSDATA"] == "" || credential.Cookies["bili_jct"] == "" || credential.Cookies["DedeUserID"] == "" {
		return nil, errors.New("Bilibili 凭据无效")
	}
	cookieAccountID := strings.TrimSpace(credential.Cookies["DedeUserID"])
	if cfg.accountID != "" && cfg.accountID != cookieAccountID {
		return nil, errors.New("Bilibili 账号 ID 与 Cookie 不匹配")
	}
	accountID := cookieAccountID
	stop, err := commentbridge.NewBilibiliRiskStop(store, send)
	if err != nil {
		return nil, errors.New("无法初始化 B 站风控停用状态")
	}
	client, err := bili.NewClient(bili.Config{RiskStop: stop})
	if err != nil {
		return nil, errors.New("Bilibili 客户端配置无效")
	}
	client.SetCredential(credential)
	adapter, err := bili.NewConfiguredCommentAdapter(client, bili.CommentAdapterConfig{AccountID: accountID, CredentialFile: path, SubReplyPages: cfg.subReplyPages})
	if err != nil {
		return nil, errors.New("Bilibili 评论配置无效")
	}
	return adapter, nil
}

func runAuth(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "youtube" {
		return errors.New("auth 仅支持 youtube")
	}
	fs := flag.NewFlagSet("auth youtube", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	clientFile := fs.String("client-file", "", "OAuth 客户端 JSON")
	tokenFile := fs.String("token-file", "", "令牌文件")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || strings.TrimSpace(*clientFile) == "" || strings.TrimSpace(*tokenFile) == "" {
		return errors.New("auth youtube 参数无效")
	}
	payload, err := load0600Secret(*clientFile)
	if err != nil {
		return errors.New("YouTube 客户端文件无效")
	}
	defer clearBytes(payload)
	credentials, err := youtube.ParseDesktopClientCredentials(payload)
	if err != nil {
		return errors.New("YouTube 客户端配置无效")
	}
	authorization, err := youtube.StartDesktopAuthorization(ctx, credentials, youtube.DesktopAuthorizationOptions{})
	if err != nil {
		return errors.New("无法启动 YouTube 授权")
	}
	defer authorization.Close()
	// This is intentionally the command's only stdout output: no code, token,
	// client secret, or explanatory text can be mixed into shell captures.
	fmt.Fprintln(stdout, authorization.AuthorizationURL)
	token, err := authorization.Wait(ctx)
	if err != nil {
		return errors.New("YouTube 授权未完成")
	}
	if err := youtube.SaveToken(*tokenFile, token); err != nil {
		return errors.New("无法保存 YouTube 令牌")
	}
	return nil
}

func runStatus(ctx context.Context, args []string, stdout io.Writer) error {
	if err := loadConfiguredEnv(); err != nil {
		return errors.New("加载环境配置失败")
	}
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dbPath := fs.String("db", defaultString(strings.TrimSpace(os.Getenv("SHADOWBRAIN_COMMENT_BRIDGE_DB")), defaultDB), "SQLite 数据库路径")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || strings.TrimSpace(*dbPath) == "" {
		return errors.New("status 参数无效")
	}
	store, err := commentbridge.OpenReadOnly(*dbPath)
	if err != nil {
		return errors.New("无法只读打开评论桥接数据库")
	}
	defer store.Close()
	counts, err := store.StatusCounts(ctx)
	if err != nil {
		return errors.New("无法读取评论桥接状态")
	}
	riskStatus, err := store.BilibiliRiskStatus(ctx)
	if err != nil {
		return err
	}
	if riskStatus != "" {
		fmt.Fprintln(stdout, riskStatus)
	}
	printCounts(stdout, "notifications", counts.Notifications)
	printCounts(stdout, "credential-alerts", counts.CredentialAlerts)
	printCounts(stdout, "credential-refreshes", counts.CredentialRefreshes)
	printCounts(stdout, "replies", counts.Replies)
	printCounts(stdout, "engagements", counts.Engagements)
	printCounts(stdout, "moderations", counts.Moderations)
	printCounts(stdout, "bot-commands", counts.BotCommands)
	printCounts(stdout, "reactions", counts.Reactions)
	printCounts(stdout, "manual-engagement-feedbacks", counts.ManualEngagementFeedbacks)
	if counts.Danmaku != nil {
		for _, status := range []commentbridge.JobStatus{"pending", "sending", "succeeded", "unknown", "failed"} {
			fmt.Fprintf(stdout, "danmaku %s=%d\n", status, counts.Danmaku[status])
		}
	}
	for _, status := range []commentbridge.JobStatus{commentbridge.JobUnknown, commentbridge.JobFailed} {
		jobs, err := store.ListTerminalJobs(ctx, status)
		if err != nil {
			return errors.New("无法读取终态动作")
		}
		for _, job := range jobs {
			printTerminalJob(stdout, job)
		}
	}
	return nil
}

func printCounts(w io.Writer, kind string, counts map[commentbridge.JobStatus]int) {
	statuses := []commentbridge.JobStatus{commentbridge.JobPending, commentbridge.JobSending, commentbridge.JobSent, commentbridge.JobUnknown, commentbridge.JobFailed}
	for _, status := range statuses {
		fmt.Fprintf(w, "%s %s=%d\n", kind, status, counts[status])
	}
}

func printTerminalJob(w io.Writer, job commentbridge.TerminalJob) {
	fmt.Fprintf(w, "action=%s kind=%s status=%s platform=%s comment=%s feishu=%s\n", job.ActionID, job.Kind, job.Status, job.Platform, job.CommentID, job.FeishuMessageID)
}

func runRetryUnknown(ctx context.Context, args []string, stdout io.Writer) error {
	if err := loadConfiguredEnv(); err != nil {
		return errors.New("加载环境配置失败")
	}
	fs := flag.NewFlagSet("retry-unknown", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dbPath := fs.String("db", defaultString(strings.TrimSpace(os.Getenv("SHADOWBRAIN_COMMENT_BRIDGE_DB")), defaultDB), "SQLite 数据库路径")
	actionID := fs.String("action", "", "unknown action ID")
	confirm := fs.Bool("confirm-possible-duplicate", false, "确认外部平台可能已成功")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || strings.TrimSpace(*dbPath) == "" || strings.TrimSpace(*actionID) == "" || !*confirm {
		return errors.New("retry-unknown 参数无效；必须明确确认可能重复")
	}
	readonly, err := commentbridge.OpenReadOnly(*dbPath)
	if err != nil {
		return errors.New("无法只读打开评论桥接数据库")
	}
	jobs, err := readonly.ListTerminalJobs(ctx, commentbridge.JobUnknown)
	_ = readonly.Close()
	if err != nil {
		return errors.New("无法读取 unknown 动作")
	}
	var target *commentbridge.TerminalJob
	for i := range jobs {
		if jobs[i].ActionID == *actionID {
			target = &jobs[i]
			break
		}
	}
	if target == nil {
		return errors.New("指定动作不是 unknown")
	}
	printTerminalJob(stdout, *target)
	fmt.Fprintln(stdout, "警告：外部平台可能已经成功；重新执行可能产生重复消息、回复或互动动作。")
	lock, err := runlock.Acquire(*dbPath)
	if err != nil {
		return errors.New("无法获取评论桥接数据库运行锁")
	}
	defer lock.Close()
	store, err := commentbridge.Open(*dbPath)
	if err != nil {
		return errors.New("无法打开评论桥接数据库")
	}
	defer store.Close()
	ok, err := store.ResetUnknownToPending(ctx, *actionID)
	if err != nil {
		return errors.New("无法重置 unknown 动作")
	}
	if !ok {
		return errors.New("指定动作不再是 unknown")
	}
	fmt.Fprintln(stdout, "已重置为 pending。")
	return nil
}
