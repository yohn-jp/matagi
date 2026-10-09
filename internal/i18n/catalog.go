// Package i18n owns the operator-facing message catalog for Matagi. English
// message IDs are the source strings, so a missing Japanese translation has a
// deterministic English fallback. Machine values and remote evidence should
// only be translated when they are known operator status vocabulary.
package i18n

import (
	"bytes"
	"fmt"
	"os"
	"strings"
)

// Locale is one supported operator interface language.
type Locale string

const (
	English  Locale = "en"
	Japanese Locale = "ja"
)

// Supported lists the selectable locales in display order.
var Supported = []Locale{English, Japanese}

// Name returns the locale's own name for an untranslated language selector.
func (l Locale) Name() string {
	if l == Japanese {
		return "日本語"
	}
	return "English"
}

// Valid reports whether s names a supported locale exactly.
func Valid(s string) bool {
	return s == string(English) || s == string(Japanese)
}

// Resolve selects an explicit supported locale first, then the first
// recognized host language tag, and otherwise English. Host tags may use
// common BCP-47, POSIX, or Windows locale separators.
func Resolve(saved string, host ...string) Locale {
	if Valid(saved) {
		return Locale(saved)
	}
	for _, tag := range host {
		language := strings.ToLower(strings.TrimSpace(tag))
		if index := strings.IndexAny(language, "-_.@ "); index >= 0 {
			language = language[:index]
		}
		if Valid(language) {
			return Locale(language)
		}
	}
	return English
}

// HostLocales lists available host locale hints. NativeLocale is provided by
// the operating-system implementation; POSIX environment variables remain
// useful on Unix and for explicitly configured environments.
func HostLocales() []string {
	var locales []string
	if native := nativeLocale(); native != "" {
		locales = append(locales, native)
	}
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if value := os.Getenv(key); value != "" && value != "C" && value != "POSIX" {
			locales = append(locales, value)
		}
	}
	return locales
}

// T renders msg in l and formats its arguments when supplied.
func (l Locale) T(msg string, args ...any) string {
	text := msg
	if l == Japanese {
		if translated, ok := ja[msg]; ok {
			text = translated
		}
	}
	if len(args) == 0 {
		return text
	}
	return fmt.Sprintf(text, args...)
}

// Has reports whether the catalog contains a Japanese translation. English
// message IDs always render as-is.
func (l Locale) Has(msg string) bool {
	if l != Japanese {
		return true
	}
	_, ok := ja[msg]
	return ok
}

// Table renders the requested message IDs for browser code that composes
// operator-facing copy after the page has loaded.
func (l Locale) Table(messages []string) map[string]string {
	table := make(map[string]string, len(messages))
	for _, message := range messages {
		table[message] = l.T(message)
	}
	return table
}

// BrowserMessages are the English source IDs used by the shared workspace
// script. Status values and remote error text are inserted literally.
var BrowserMessages = []string{
	"Live · updated ",
	"status stale · refresh failed",
	"Status is live again.",
	"Status is stale: refresh failed.",
	"Connecting to SSH and checking Jinushi…",
	"Ensuring endpoint…",
	"Working…",
	"Saving…",
	"Checking…",
	"Starting download…",
	"Restarting…",
}

// OperatorMessages lists every English source string used by Matagi's normal
// operator surfaces. It also gives tests a complete catalog-coverage check.
var OperatorMessages = []string{
	"Skip to content",
	"Live · updates every 3 seconds",
	"Refresh",
	"Language",
	"Set language",
	"Workspace",
	"Settings",
	"Updates",
	"Matagi navigation",
	"Service views",
	"Select view: %s · %s · %s",
	"Selected service view actions",
	"No service tabs are open.",
	"Parked",
	"Window view",
	"Opening",
	"Moving",
	"Open failed",
	"Unavailable",
	"Move to window",
	"Return to tabs",
	"Resume",
	"Close view",
	"Open in window",
	"Registered environments and service endpoints.",
	"Select environment",
	"Select environment: %s · OpenSSH alias %s · connectivity %s",
	"Display language",
	"Matagi operator preferences.",
	"Saved service views",
	"Saved views reopen as parked placeholders. Matagi does not load a saved address or ensure an endpoint until you choose Resume.",
	"Reset saved layout",
	"Saved layout controls are unavailable.",
	"Presentation controls are unavailable.",
	"Choose a valid view location.",
	"A valid service view is required.",
	"The service view could not be updated. Refresh the workspace and try again.",
	"This saved view is parked. Resume checks the registered endpoint before opening it.",
	"This registered endpoint is being ensured.",
	"This service view is changing location.",
	"This view could not be opened. Check the workspace status, then try Resume.",
	"This view is unavailable. Resume checks the registered endpoint again.",
	"This service view is unavailable.",
	"The saved layout could not be reset. Workspace and open views are unchanged.",
	"Development environments",
	"Connection · SSH",
	"OpenSSH alias",
	"Jinushi · process supervisor",
	"Check Jinushi",
	"Services",
	"Managed through Jinushi",
	"No services are registered yet. Add a service to start operating this environment.",
	"Desired",
	"Process",
	"Readiness",
	"Process:",
	"Readiness:",
	"Start",
	"Restart",
	"Stop",
	"Tunnel:",
	"Open",
	"Add service",
	"Service name",
	"This name identifies the service in Matagi.",
	"Remote UI port (static endpoint)",
	"Dynamic endpoint descriptor path (optional)",
	"Use an absolute remote path to a JSON file with a url field. Matagi checks it against the managed Run output.",
	"Readiness path",
	"(optional; defaults to /)",
	"Working directory on the development host",
	"Start command and arguments",
	"Separate arguments with spaces. Use a wrapper script when an argument requires quoting; Matagi does not parse shell quoting.",
	"The command runs through Jinushi on the development host. Matagi forwards only the registered loopback UI port.",
	"Connect a development environment",
	"Use the SSH host alias from your existing OpenSSH configuration. Matagi checks SSH and Jinushi before saving the environment.",
	"Environment name",
	"SSH host alias",
	"Advanced · Jinushi bootstrap",
	"Bootstrap command and arguments",
	"(only if Jinushi is not running)",
	"Leave blank when Jinushi is already running. An explicit command runs only if it is unavailable.",
	"Connect environment",
	"Loading the Matagi workspace…",
	"Request could not be completed",
	"Return to Workspace",
	"Workspace status is unavailable.",
	"The service name is required.",
	"Enter a valid UI port (1–65535).",
	"Choose English or Japanese.",
	"Invalid form submission.",
	"Invalid form token.",
	"Page not found.",
	"An environment is required.",
	"An environment and service are required.",
	"Unknown service action.",
	"An environment, service, and endpoint are required.",
	"The ensured endpoint did not return a permitted loopback URL.",
	"method not allowed",
	"Language preference settings are unavailable.",
	"The language preference could not be saved.",
	"Check the entered details and try again. No changes were saved.",
	"SSH transport or authentication failed. Check your OpenSSH host alias and credentials.",
	"The SSH host did not respond before the connection timed out.",
	"SSH connected, but the remote connectivity check failed.",
	"The local OpenSSH client could not complete the request. Check its installation and configuration.",
	"Jinushi is unavailable. Check that it is installed, or specify its bootstrap command under Advanced.",
	"Jinushi did not respond before the request timed out. Check Jinushi on the development host.",
	"Jinushi rejected the lifecycle action or returned a failed result. Check its response and the service registration.",
	"Jinushi returned an invalid response. Check Jinushi on the development host.",
	"This environment or service is already registered or changing state. Refresh the workspace.",
	"The operation was canceled.",
	"The environment or service is no longer registered. Refresh the workspace.",
	"The lifecycle action could not be confirmed. Refresh the workspace and inspect the managed process before retrying this action.",
	"The endpoint could not be ensured. Check Jinushi, service readiness, and the tunnel status.",
	"The application endpoint could not be resolved or reached. Check its descriptor, managed Run output, and service status.",
	"The desktop navigation policy is unavailable.",
	"The ensured endpoint did not match the requested endpoint.",
	"The endpoint is not locally available.",
	"This service has no managed process evidence yet.",
	"Matagi could not start",
	"Manual",
	"The update channel was saved. No network request was made.",
	"Installed",
	"Installed version",
	"unknown (identified by an explicit check)",
	"Executable SHA-256",
	"Last check",
	"latest",
	"failed",
	"never",
	"Update channel",
	"Stable",
	"Development",
	"Save channel",
	"Stable offers stable releases (X.Y.Z). Development offers development releases (X.Y.Z-dev) and stable releases. The channel only selects which releases an explicit check offers: it never downloads, installs or downgrades anything.",
	"Manual, verified updates of the Matagi executable. Opening this page and changing the channel contact nothing; only Check for updates and Download use the network.",
	"An update action is in progress. Check, Download, Save channel and Restart & update are unavailable until it finishes; progress is shown in Download and verification.",
	"Show progress",
	"Releases",
	"Official Matagi GitHub Releases",
	"Check for updates",
	"Retrieves the release list from GitHub, only now. Results below stay until you check again, change the channel or restart Matagi.",
	"Checked",
	"channel",
	"Status",
	"Published",
	"Actions",
	"newer",
	"installed",
	"older",
	"not comparable",
	"Not offered: an update never goes backwards.",
	"Download",
	"The installed version is not known, so releases cannot be compared with it. Download only a release you intend to install.",
	"No eligible releases on this channel.",
	"Download and verification",
	"No download in progress.",
	"Ready to install",
	"Release",
	"Verified",
	"matches the release checksum",
	"Replaces",
	"Restart & update is available only in the Windows desktop application.",
	"Close Matagi, replace the executable and reopen it? Matagi-owned settings, registered environments and services remain stored.",
	"Restart & update",
	"The executable is replaced by a separate helper after Matagi exits. If replacement fails, the previous executable is kept and reopened.",
	"Nothing downloaded and verified yet.",
	"Last update attempt",
	"UPDATED",
	"RESTART FAILED",
	"NOT UPDATED",
	"RUNNING",
	"DONE",
	"FAILED",
	"update check",
	"Download update",
	"Starting",
	"Fetching",
	"Release list",
	"Checksum file",
	"Verification",
	"Downloading",
	"Verifying",
	"Working",
	"phase",
	"of",
	"received",
	"Checking…",
	"Starting download…",
	"Restarting…",
	"Nothing was downloaded and the installed executable was not changed.",
	"Nothing was downloaded.",
	"The partial download was discarded.",
	"The downloaded file failed verification and was discarded.",
	"The downloaded file was not put in place.",
	"A previously verified update is still ready to install.",
	"No update is ready to install.",
	"The update was downloaded and verified. Next: Restart & update, under Ready to install.",
	"Go to Ready to install",
	"Check failed",
	"Download failed",
	"Applied",
	"Restart failed",
	"No update action is in progress.",
	"Matagi could not read the release list from GitHub. Check the network connection and try Check for updates again. The installed Matagi is unchanged.",
	"GitHub's release data is not in the shape the release workflow publishes, so nothing was offered. The installed Matagi is unchanged.",
	"The release's checksum file is missing or not usable, so the download was discarded. Nothing was installed.",
	"The downloaded file does not match the release's checksum, so it was discarded. Nothing was installed; Download can be retried.",
	"Matagi could not read the official release list from GitHub. Check the network connection and try Check for updates again. The installed Matagi executable is unchanged.",
	"GitHub's release data is malformed, so nothing was offered. The installed Matagi executable is unchanged.",
	"This release does not carry exactly one Windows executable and one checksum file. Choose another release or try again later.",
	"The executable could not be downloaded. Nothing was installed; Download can be retried.",
	"The release checksum file is missing or unusable, so the download was discarded. Nothing was installed.",
	"The downloaded file does not match the release checksum, so it was discarded. Nothing was installed; Download can be retried.",
	"The executable could not be replaced. The previous executable was kept or restored; Matagi keeps running from it.",
	"The executable was replaced but could not be restarted. Start Matagi again from the same path.",
	"Saving…",
	"Skipped",
	"Waiting",
	"Failed in phase",
	"Phase %d of %d",
	"received (the server did not state a total size)",
	"The update action could not be completed.",
	"Choose Stable or Development.",
	"Another update action is in progress. Wait for it to finish.",
	"This action is unavailable in the current update state. Review the selected channel, ready candidate and operation status before retrying.",
	"This candidate is not installable. Review the status and error detail above, then check for updates or download another verified candidate.",
	"The updater could not read or save local update state. Check Matagi's state directory and review the error detail.",
	"The update attempt was refused. Review the current update status and error detail before retrying.",
	"unknown",
	"connected",
	"disconnected",
	"unreachable",
	"ready",
	"not-ready",
	"starting",
	"running",
	"stopped",
	"failed",
	"healthy",
	"unhealthy",
	"available",
	"unavailable",
	"ensuring",
	"pending",
	"error",
	"User interface",
}

var ja = map[string]string{
	"Skip to content":                         "本文へ移動",
	"Live · updates every 3 seconds":          "ライブ · 3 秒ごとに更新",
	"Live · updated ":                         "ライブ · 更新時刻 ",
	"status stale · refresh failed":           "状態が古くなっています · 更新に失敗しました",
	"Status is live again.":                   "状態のライブ更新が再開しました。",
	"Status is stale: refresh failed.":        "更新に失敗し、状態が古くなっています。",
	"Connecting to SSH and checking Jinushi…": "SSH に接続して Jinushi を確認中…",
	"Ensuring endpoint…":                      "エンドポイントを準備中…",
	"Working…":                                "処理中…",
	"Refresh":                                 "更新",
	"Language":                                "表示言語",
	"Set language":                            "言語を設定",
	"Workspace":                               "ワークスペース",
	"Settings":                                "設定",
	"Updates":                                 "更新",
	"Matagi navigation":                       "Matagi ナビゲーション",
	"Service views":                           "サービス画面",
	"Select view: %s · %s · %s":               "画面を選択: %s · %s · %s",
	"Selected service view actions":           "選択中のサービス画面の操作",
	"No service tabs are open.":               "開いているサービスのタブはありません。",
	"Parked":                                  "休止中",
	"Window view":                             "ウィンドウの画面",
	"Opening":                                 "表示中",
	"Moving":                                  "移動中",
	"Open failed":                             "表示失敗",
	"Unavailable":                             "利用不可",
	"Move to window":                          "ウィンドウへ移動",
	"Return to tabs":                          "タブに戻す",
	"Resume":                                  "再開",
	"Close view":                              "画面を閉じる",
	"Open in window":                          "ウィンドウで開く",
	"Registered environments and service endpoints.":              "登録済みの開発環境とサービスのエンドポイント。",
	"Select environment":                                          "環境を選択",
	"Select environment: %s · OpenSSH alias %s · connectivity %s": "環境を選択: %s · OpenSSH エイリアス %s · 接続状態 %s",
	"Display language":                                            "表示言語",
	"Matagi operator preferences.":                                "Matagi の表示設定。",
	"Saved service views":                                         "保存済みのサービス画面",
	"Saved views reopen as parked placeholders. Matagi does not load a saved address or ensure an endpoint until you choose Resume.": "保存した画面は休止中のプレースホルダーとして開きます。再開を選ぶまで保存済みのアドレスを読み込んだり、エンドポイントを準備したりしません。",
	"Reset saved layout":                                                                  "保存済みレイアウトをリセット",
	"Saved layout controls are unavailable.":                                              "保存済みレイアウトの操作は利用できません。",
	"Presentation controls are unavailable.":                                              "画面操作を利用できません。",
	"Choose a valid view location.":                                                       "有効な画面の配置先を選択してください。",
	"A valid service view is required.":                                                   "有効なサービス画面を指定してください。",
	"The service view could not be updated. Refresh the workspace and try again.":         "サービス画面を更新できませんでした。ワークスペースを更新して再試行してください。",
	"This saved view is parked. Resume checks the registered endpoint before opening it.": "保存済みの画面は休止中です。再開すると、登録済みのエンドポイントを確認してから開きます。",
	"This registered endpoint is being ensured.":                                          "登録済みのエンドポイントを準備しています。",
	"This service view is changing location.":                                             "サービス画面の配置を変更しています。",
	"This view could not be opened. Check the workspace status, then try Resume.":         "画面を開けませんでした。ワークスペースの状態を確認してから再開してください。",
	"This view is unavailable. Resume checks the registered endpoint again.":              "この画面は利用できません。再開すると、登録済みのエンドポイントを再確認します。",
	"This service view is unavailable.":                                                   "このサービス画面は利用できません。",
	"The saved layout could not be reset. Workspace and open views are unchanged.":        "保存済みレイアウトをリセットできませんでした。ワークスペースと開いている画面は変更されていません。",
	"Development environments":                                                            "開発環境",
	"Connection · SSH":                                                                    "接続 · SSH",
	"OpenSSH alias":                                                                       "OpenSSH エイリアス",
	"Jinushi · process supervisor":                                                        "Jinushi · プロセス管理",
	"Check Jinushi":                                                                       "Jinushi を確認",
	"Services":                                                                            "サービス",
	"Managed through Jinushi":                                                             "Jinushi で管理",
	"No services are registered yet. Add a service to start operating this environment.":  "サービスはまだ登録されていません。この環境を操作するにはサービスを追加してください。",
	"Desired":      "目標状態",
	"Process":      "プロセス",
	"Readiness":    "準備状態",
	"Process:":     "プロセス:",
	"Readiness:":   "準備状態:",
	"Start":        "開始",
	"Restart":      "再起動",
	"Stop":         "停止",
	"Tunnel:":      "トンネル:",
	"Open":         "開く",
	"Add service":  "サービスを追加",
	"Service name": "サービス名",
	"This name identifies the service in Matagi.": "この名前で Matagi 上のサービスを識別します。",
	"Remote UI port (static endpoint)":            "リモート UI ポート（固定エンドポイント）",
	"Dynamic endpoint descriptor path (optional)": "動的エンドポイントの記述ファイルパス（省略可）",
	"Use an absolute remote path to a JSON file with a url field. Matagi checks it against the managed Run output.": "url フィールドを含む JSON ファイルの絶対リモートパスを指定してください。Matagi は管理対象 Run の出力と照合します。",
	"Readiness path":                            "準備状態の確認パス",
	"(optional; defaults to /)":                 "（省略可。既定値は /）",
	"Working directory on the development host": "開発ホスト上の作業ディレクトリ",
	"Start command and arguments":               "起動コマンドと引数",
	"Separate arguments with spaces. Use a wrapper script when an argument requires quoting; Matagi does not parse shell quoting.": "引数は空白で区切ってください。引用符が必要な引数にはラッパースクリプトを使用してください。Matagi はシェルの引用符を解析しません。",
	"The command runs through Jinushi on the development host. Matagi forwards only the registered loopback UI port.":              "コマンドは開発ホスト上で Jinushi を通じて実行されます。Matagi が転送するのは登録済みのループバック UI ポートのみです。",
	"Connect a development environment": "開発環境を接続",
	"Use the SSH host alias from your existing OpenSSH configuration. Matagi checks SSH and Jinushi before saving the environment.": "既存の OpenSSH 設定にある SSH ホストエイリアスを使用してください。Matagi は環境を保存する前に SSH と Jinushi を確認します。",
	"Environment name":                 "環境名",
	"SSH host alias":                   "SSH ホストエイリアス",
	"Advanced · Jinushi bootstrap":     "詳細 · Jinushi の起動設定",
	"Bootstrap command and arguments":  "起動準備コマンドと引数",
	"(only if Jinushi is not running)": "（Jinushi が起動していない場合のみ）",
	"Leave blank when Jinushi is already running. An explicit command runs only if it is unavailable.": "Jinushi がすでに起動している場合は空欄にしてください。明示したコマンドは Jinushi を利用できない場合のみ実行します。",
	"Connect environment":                                             "環境を接続",
	"Loading the Matagi workspace…":                                   "Matagi ワークスペースを読み込み中…",
	"Request could not be completed":                                  "リクエストを完了できませんでした",
	"Return to Workspace":                                             "ワークスペースに戻る",
	"Workspace status is unavailable.":                                "ワークスペースの状態を取得できません。",
	"The service name is required.":                                   "サービス名を入力してください。",
	"Enter a valid UI port (1–65535).":                                "有効な UI ポート（1～65535）を入力してください。",
	"Choose English or Japanese.":                                     "English または 日本語を選択してください。",
	"Language preference settings are unavailable.":                   "表示言語の設定を利用できません。",
	"The language preference could not be saved.":                     "表示言語を保存できませんでした。",
	"Check the entered details and try again. No changes were saved.": "入力内容を確認して、もう一度お試しください。変更は保存されていません。",
	"SSH transport or authentication failed. Check your OpenSSH host alias and credentials.":                              "SSH の通信または認証に失敗しました。OpenSSH のホストエイリアスと認証情報を確認してください。",
	"The SSH host did not respond before the connection timed out.":                                                       "タイムアウトまでに SSH ホストから応答がありませんでした。",
	"SSH connected, but the remote connectivity check failed.":                                                            "SSH には接続しましたが、リモートの接続確認に失敗しました。",
	"The local OpenSSH client could not complete the request. Check its installation and configuration.":                  "ローカルの OpenSSH クライアントが要求を完了できませんでした。インストールと設定を確認してください。",
	"Jinushi is unavailable. Check that it is installed, or specify its bootstrap command under Advanced.":                "Jinushi を利用できません。インストールを確認するか、詳細設定で起動準備コマンドを指定してください。",
	"Jinushi did not respond before the request timed out. Check Jinushi on the development host.":                        "要求がタイムアウトするまでに Jinushi から応答がありませんでした。開発ホスト上の Jinushi を確認してください。",
	"Jinushi rejected the lifecycle action or returned a failed result. Check its response and the service registration.": "Jinushi がライフサイクル操作を拒否したか、失敗を返しました。応答とサービス登録を確認してください。",
	"Jinushi returned an invalid response. Check Jinushi on the development host.":                                        "Jinushi から無効な応答が返されました。開発ホスト上の Jinushi を確認してください。",
	"This environment or service is already registered or changing state. Refresh the workspace.":                         "この環境またはサービスはすでに登録済みか、状態を変更中です。ワークスペースを更新してください。",
	"The operation was canceled.": "操作はキャンセルされました。",
	"The environment or service is no longer registered. Refresh the workspace.":                                                      "環境またはサービスの登録が見つかりません。ワークスペースを更新してください。",
	"The lifecycle action could not be confirmed. Refresh the workspace and inspect the managed process before retrying this action.": "ライフサイクル操作の結果を確認できませんでした。ワークスペースを更新し、管理対象プロセスの状態を確認してから再試行してください。",
	"The endpoint could not be ensured. Check Jinushi, service readiness, and the tunnel status.":                                     "エンドポイントを準備できませんでした。Jinushi、サービスの準備状態、トンネルの状態を確認してください。",
	"The application endpoint could not be resolved or reached. Check its descriptor, managed Run output, and service status.":        "アプリケーションのエンドポイントを解決または接続できませんでした。エンドポイント記述子、管理対象 Run の出力、サービス状態を確認してください。",
	"The desktop navigation policy is unavailable.":                                                                                   "デスクトップのナビゲーションポリシーを利用できません。",
	"The ensured endpoint did not match the requested endpoint.":                                                                      "準備されたエンドポイントが要求したものと一致しません。",
	"The endpoint is not locally available.":                                                                                          "エンドポイントをローカルで利用できません。",
	"This service has no managed process evidence yet.":                                                                               "このサービスには管理対象プロセスの証拠がまだありません。",
	"Invalid form submission.":                                      "フォームの送信内容が無効です。",
	"Invalid form token.":                                           "フォームトークンが無効です。",
	"Page not found.":                                               "ページが見つかりません。",
	"An environment is required.":                                   "環境を指定してください。",
	"An environment and service are required.":                      "環境とサービスを指定してください。",
	"Unknown service action.":                                       "不明なサービス操作です。",
	"An environment, service, and endpoint are required.":           "環境、サービス、エンドポイントを指定してください。",
	"The ensured endpoint did not return a permitted loopback URL.": "準備されたエンドポイントが許可されたループバック URL を返しませんでした。",
	"method not allowed":                                            "許可されていないメソッドです。",
	"Matagi could not start":                                        "Matagi を起動できませんでした",
	"Manual":                                                        "手動",
	"The update channel was saved. No network request was made.":    "更新チャンネルを保存しました。ネットワーク通信は行っていません。",
	"Installed":         "インストール済み",
	"Installed version": "インストール済みバージョン",
	"unknown (identified by an explicit check)": "不明（明示的な確認で識別）",
	"Executable SHA-256":                        "実行ファイルの SHA-256",
	"Last check":                                "前回の確認",
	"latest":                                    "最新",
	"never":                                     "未実施",
	"Update channel":                            "更新チャンネル",
	"Stable":                                    "安定版",
	"Development":                               "開発版",
	"Save channel":                              "チャンネルを保存",
	"Stable offers stable releases (X.Y.Z). Development offers development releases (X.Y.Z-dev) and stable releases. The channel only selects which releases an explicit check offers: it never downloads, installs or downgrades anything.": "安定版では安定リリース（X.Y.Z）を提供します。開発版では開発リリース（X.Y.Z-dev）と安定リリースを提供します。チャンネルは明示的な確認で表示するリリースを選ぶだけです。ダウンロード、インストール、ダウングレードは行いません。",
	"Manual, verified updates of the Matagi executable. Opening this page and changing the channel contact nothing; only Check for updates and Download use the network.":                                                                    "Matagi 実行ファイルを手動で確認・検証して更新します。このページを開いたりチャンネルを変更したりしても通信しません。ネットワークを使うのは「更新を確認」と「ダウンロード」のみです。",
	"An update action is in progress. Check, Download, Save channel and Restart & update are unavailable until it finishes; progress is shown in Download and verification.":                                                                 "更新処理中です。完了するまで確認、ダウンロード、チャンネル保存、再起動して更新は利用できません。進行状況はダウンロードと検証に表示されます。",
	"Show progress":                   "進行状況を表示",
	"Releases":                        "リリース",
	"Official Matagi GitHub Releases": "Matagi 公式 GitHub リリース",
	"Check for updates":               "更新を確認",
	"Retrieves the release list from GitHub, only now. Results below stay until you check again, change the channel or restart Matagi.": "GitHub からリリース一覧を取得します。通信するのは今だけです。再確認、チャンネル変更、Matagi の再起動まで結果を保持します。",
	"Checked":        "確認日時",
	"channel":        "チャンネル",
	"Status":         "状態",
	"Published":      "公開日時",
	"Actions":        "操作",
	"newer":          "新しい",
	"installed":      "インストール済み",
	"older":          "古い",
	"not comparable": "比較不可",
	"Not offered: an update never goes backwards.": "ダウングレードになるため提供されません。",
	"Download": "ダウンロード",
	"The installed version is not known, so releases cannot be compared with it. Download only a release you intend to install.": "インストール済みバージョンが不明なため、リリースを比較できません。インストールするリリースだけをダウンロードしてください。",
	"No eligible releases on this channel.": "このチャンネルに対象のリリースはありません。",
	"Download and verification":             "ダウンロードと検証",
	"No download in progress.":              "ダウンロードは進行していません。",
	"Ready to install":                      "インストール準備完了",
	"Release":                               "リリース",
	"Verified":                              "検証済み",
	"matches the release checksum":          "リリースのチェックサムと一致",
	"Replaces":                              "置き換え対象",
	"Restart & update is available only in the Windows desktop application.":                                                         "再起動して更新できるのは Windows デスクトップアプリのみです。",
	"Close Matagi, replace the executable and reopen it? Matagi-owned settings, registered environments and services remain stored.": "Matagi を終了して実行ファイルを置き換え、再起動しますか？Matagi の設定、登録済み環境、サービスは保持されます。",
	"Restart & update": "再起動して更新",
	"The executable is replaced by a separate helper after Matagi exits. If replacement fails, the previous executable is kept and reopened.": "Matagi の終了後、別のヘルパーが実行ファイルを置き換えます。置き換えに失敗した場合は以前の実行ファイルを保持して再度起動します。",
	"Nothing downloaded and verified yet.": "検証済みのダウンロードはまだありません。",
	"Last update attempt":                  "前回の更新結果",
	"UPDATED":                              "更新済み",
	"RESTART FAILED":                       "再起動失敗",
	"NOT UPDATED":                          "未更新",
	"RUNNING":                              "実行中",
	"DONE":                                 "完了",
	"FAILED":                               "失敗",
	"update check":                         "更新確認",
	"Download update":                      "更新をダウンロード",
	"Starting":                             "開始中",
	"Fetching":                             "取得中",
	"Release list":                         "リリース一覧",
	"Checksum file":                        "チェックサムファイル",
	"Verification":                         "検証",
	"Downloading":                          "ダウンロード中",
	"Verifying":                            "検証中",
	"Working":                              "処理中",
	"phase":                                "フェーズ",
	"of":                                   "/",
	"received":                             "受信済み",
	"Checking…":                            "確認中…",
	"Starting download…":                   "ダウンロードを開始中…",
	"Restarting…":                          "再起動中…",
	"Nothing was downloaded and the installed executable was not changed.": "ダウンロードは行われず、インストール済みの実行ファイルも変更されていません。",
	"Nothing was downloaded.":                                                                 "ダウンロードは行われませんでした。",
	"The partial download was discarded.":                                                     "不完全なダウンロードを破棄しました。",
	"The downloaded file failed verification and was discarded.":                              "ダウンロードしたファイルの検証に失敗したため、破棄しました。",
	"The downloaded file was not put in place.":                                               "ダウンロードしたファイルは配置されませんでした。",
	"A previously verified update is still ready to install.":                                 "以前に検証した更新は引き続きインストールできます。",
	"No update is ready to install.":                                                          "インストールできる更新はありません。",
	"The update was downloaded and verified. Next: Restart & update, under Ready to install.": "更新をダウンロードして検証しました。次は「インストール準備完了」から「再起動して更新」を実行してください。",
	"Go to Ready to install":                                                                  "「インストール準備完了」へ移動",
	"Check failed":                                                                            "確認に失敗",
	"Download failed":                                                                         "ダウンロードに失敗",
	"Applied":                                                                                 "適用済み",
	"Restart failed":                                                                          "再起動に失敗",
	"No update action is in progress.":                                                        "更新処理は進行していません。",
	"Matagi could not read the official release list from GitHub. Check the network connection and try Check for updates again. The installed Matagi executable is unchanged.": "Matagi は GitHub から公式リリース一覧を取得できませんでした。ネットワーク接続を確認して、もう一度「更新を確認」してください。インストール済みの Matagi 実行ファイルは変更されていません。",
	"GitHub's release data is malformed, so nothing was offered. The installed Matagi executable is unchanged.":                                                                "GitHub のリリース情報が不正なため、候補を表示しませんでした。インストール済みの Matagi 実行ファイルは変更されていません。",
	"This release does not carry exactly one Windows executable and one checksum file. Choose another release or try again later.":                                             "このリリースには Windows 実行ファイルまたはチェックサムファイルが一つずつ含まれていません。別のリリースを選ぶか、後でもう一度お試しください。",
	"The executable could not be downloaded. Nothing was installed; Download can be retried.":                                                                                  "実行ファイルをダウンロードできませんでした。インストールは行われていません。「ダウンロード」を再試行できます。",
	"The release checksum file is missing or unusable, so the download was discarded. Nothing was installed.":                                                                  "リリースのチェックサムファイルがないか利用できないため、ダウンロードを破棄しました。インストールは行われていません。",
	"The downloaded file does not match the release checksum, so it was discarded. Nothing was installed; Download can be retried.":                                            "ダウンロードしたファイルがリリースのチェックサムと一致しないため、破棄しました。インストールは行われていません。「ダウンロード」を再試行できます。",
	"The executable could not be replaced. The previous executable was kept or restored; Matagi keeps running from it.":                                                        "実行ファイルを置き換えられませんでした。以前の実行ファイルを保持または復元し、Matagi はそのファイルで動作を続けています。",
	"The executable was replaced but could not be restarted. Start Matagi again from the same path.":                                                                           "実行ファイルは置き換えられましたが、再起動できませんでした。同じパスから Matagi を起動してください。",
	"Matagi could not read the release list from GitHub. Check the network connection and try Check for updates again. The installed Matagi is unchanged.":                     "Matagi は GitHub からリリース一覧を取得できませんでした。ネットワーク接続を確認し、「更新を確認」をもう一度実行してください。インストール済みの Matagi は変更されていません。",
	"GitHub's release data is not in the shape the release workflow publishes, so nothing was offered. The installed Matagi is unchanged.":                                     "GitHub のリリース情報が公開ワークフローの形式と一致しないため、候補を表示しませんでした。インストール済みの Matagi は変更されていません。",
	"The release's checksum file is missing or not usable, so the download was discarded. Nothing was installed.":                                                              "リリースのチェックサムファイルが見つからないか使用できないため、ダウンロードを破棄しました。インストールは行われていません。",
	"The downloaded file does not match the release's checksum, so it was discarded. Nothing was installed; Download can be retried.":                                          "ダウンロードしたファイルがリリースのチェックサムと一致しないため、破棄しました。インストールは行われていません。「ダウンロード」を再試行できます。",
	"Saving…":         "保存中…",
	"Skipped":         "スキップ",
	"Waiting":         "待機中",
	"Failed in phase": "失敗したフェーズ:",
	"Phase %d of %d":  "フェーズ %d / %d",
	"received (the server did not state a total size)":             "受信済み（サーバーから全体サイズは通知されていません）",
	"The update action could not be completed.":                    "更新処理を完了できませんでした。",
	"Choose Stable or Development.":                                "安定版または開発版を選択してください。",
	"Another update action is in progress. Wait for it to finish.": "別の更新処理が進行中です。完了するまでお待ちください。",
	"This action is unavailable in the current update state. Review the selected channel, ready candidate and operation status before retrying.":  "現在の更新状態ではこの操作を利用できません。選択中のチャンネル、準備済み候補、処理状況を確認してから再試行してください。",
	"This candidate is not installable. Review the status and error detail above, then check for updates or download another verified candidate.": "この候補はインストールできません。上の状態とエラー詳細を確認してから、更新を確認するか、検証済みの別の候補をダウンロードしてください。",
	"The updater could not read or save local update state. Check Matagi's state directory and review the error detail.":                          "更新機能がローカルの更新状態を読み取るか保存できませんでした。Matagi の状態ディレクトリを確認し、エラー詳細を確認してください。",
	"The update attempt was refused. Review the current update status and error detail before retrying.":                                          "更新操作が拒否されました。現在の更新状態とエラー詳細を確認してから再試行してください。",
	"unknown":        "不明",
	"connected":      "接続済み",
	"disconnected":   "切断",
	"unreachable":    "到達不能",
	"ready":          "準備完了",
	"not-ready":      "未準備",
	"starting":       "起動中",
	"running":        "実行中",
	"stopped":        "停止",
	"failed":         "失敗",
	"healthy":        "正常",
	"unhealthy":      "異常",
	"available":      "利用可能",
	"unavailable":    "利用不可",
	"ensuring":       "準備中",
	"pending":        "保留中",
	"error":          "エラー",
	"User interface": "ユーザーインターフェース",
}

// FormatVerbs returns the formatting verbs in a catalog string. It is kept
// private and used by tests to ensure translation arguments stay aligned.
func formatVerbs(value string) []byte {
	var verbs bytes.Buffer
	for i := 0; i < len(value); i++ {
		if value[i] != '%' || i+1 >= len(value) {
			continue
		}
		j := i + 1
		for j < len(value) && strings.ContainsRune(".0123456789", rune(value[j])) {
			j++
		}
		if j < len(value) {
			verbs.WriteString(value[i : j+1])
		}
		i = j
	}
	return verbs.Bytes()
}
