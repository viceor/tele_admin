// Package i18n provides minimal English/Chinese message catalogs.
package i18n

import "strings"

// Language is an ISO-639-1 interface language code.
type Language string

const (
	English Language = "en"
	Chinese Language = "zh"
)

// MessageKey identifies a translatable string.
type MessageKey string

const (
	KeyStart            MessageKey = "start"
	KeyHelp             MessageKey = "help"
	KeyWelcomeNewMember MessageKey = "welcome_new_member"
	KeySpamWarning      MessageKey = "spam_warning"
	KeyBlockedKeyword   MessageKey = "blocked_keyword"
	KeyNotAdmin         MessageKey = "not_admin"
	KeyLanguageSet      MessageKey = "language_set"
	KeyTimezoneSet      MessageKey = "timezone_set"
	KeyMemberUsage      MessageKey = "member_usage"
	KeyMemberRestricted MessageKey = "member_restricted"
	KeyMemberUnrestrict MessageKey = "member_unrestricted"
	KeyMemberProtected  MessageKey = "member_protected"
	KeyMemberFailed     MessageKey = "member_action_failed"
	// KeyMemberNotApplicable rejects member actions on statuses the action
	// does not apply to, such as banned (kicked) or left users (MYC-55 SEC-1).
	KeyMemberNotApplicable MessageKey = "member_action_not_applicable"

	KeyCardUsage        MessageKey = "card_usage"
	KeyCardCreated      MessageKey = "card_created"
	KeyCardExists       MessageKey = "card_already_exists"
	KeyCardNotFound     MessageKey = "card_not_found"
	KeyCardUpdated      MessageKey = "card_updated"
	KeyCardForbidden    MessageKey = "card_forbidden"
	KeyCardAnonymous    MessageKey = "card_anonymous"
	KeyCardFieldEmpty   MessageKey = "card_field_empty"
	KeyCardFieldTooLong MessageKey = "card_field_too_long"
	KeyCardUnknownField MessageKey = "card_unknown_field"
	KeyCardStrayToken   MessageKey = "card_stray_token"
	KeyCardSearchNone   MessageKey = "card_search_none"
	// KeyCardSearchMore tells the user that a search matched more cards than
	// the per-search reply cap shows, and that narrowing the keyword will
	// find the rest (MYC-62 P2).
	KeyCardSearchMore MessageKey = "card_search_more"
	KeyCardFailed     MessageKey = "card_action_failed"

	KeyPublishUsage          MessageKey = "publish_usage"
	KeyPublishNoTargets      MessageKey = "publish_no_targets"
	KeyPublishTooManyTargets MessageKey = "publish_too_many_targets"
	KeyPublishTextTooLong    MessageKey = "publish_text_too_long"
	KeyPublishCaptionTooLong MessageKey = "publish_caption_too_long"
	KeyPublishBadMediaRef    MessageKey = "publish_bad_media_ref"
	KeyPublishUnregistered   MessageKey = "publish_unregistered_target"
	KeyPublishPrivateChat    MessageKey = "publish_private_chat"
	KeyPublishRegistered     MessageKey = "publish_registered"
	KeyPublishAlreadyReg     MessageKey = "publish_already_registered"
	KeyPublishRemoveOk       MessageKey = "publish_remove_ok"
	KeyPublishNotRegistered  MessageKey = "publish_not_registered"
	KeyPublishAllOk          MessageKey = "publish_all_ok"
	KeyPublishPartial        MessageKey = "publish_partial"
	KeyPublishFailed         MessageKey = "publish_failed"
	KeyPublishReasonNoRight  MessageKey = "publish_reason_no_right"
	KeyPublishReasonLookup   MessageKey = "publish_reason_lookup"
	KeyPublishReasonSend     MessageKey = "publish_reason_send"
	KeyPublishReasonTimeout  MessageKey = "publish_reason_timeout"
	KeyPublishNotRecorded    MessageKey = "publish_sent_not_recorded"

	KeyPersistFailed MessageKey = "persist_failed"

	KeyCloneUsage         MessageKey = "clone_usage"
	KeyCloneSame          MessageKey = "clone_same"
	KeyCloneNotAdmin      MessageKey = "clone_not_admin"
	KeyCloneDone          MessageKey = "clone_done"
	KeyTemplateUsage      MessageKey = "template_usage"
	KeyTemplateNameBad    MessageKey = "template_name_bad"
	KeyTemplateNotFound   MessageKey = "template_not_found"
	KeyTemplateSaved      MessageKey = "template_saved"
	KeyTemplateApplied    MessageKey = "template_applied"
	KeyTemplateBadJSON    MessageKey = "template_bad_json"
	KeyTemplateBadVersion MessageKey = "template_bad_version"
	KeyTemplateInvalid    MessageKey = "template_invalid"
)

var catalogs = map[Language]map[MessageKey]string{
	English: {
		KeyStart:                 "Hello! I am the group management bot. Send /help to see what I can do.",
		KeyHelp:                  "Available commands:\n/start - show help\n/help - show this message\n/welcome - greet new members\n/lang - set language (en, zh)\n/tz - set IANA timezone\n/restrict [user_id] - restrict a member (reply or user ID)\n/unrestrict [user_id] - lift a member restriction (reply or user ID)\n/card - AI business card: create, show, search, edit\n/publish <kind> <targets> <content> - send text/photo/video/document to registered chats (admins only)\n/publish_add - register this chat as a publish target (admins only)\n/publish_remove - unregister this chat as a publish target (admins only)\n/clone <source_chat_id> - copy another chat's configuration and rules to this chat (admins of both chats)\n/template - save/apply/export/import configuration templates (admins only)",
		KeyCardUsage:             "Card commands:\n/card create <key=value>... - create your card (required: name; optional: title, skills, contact, bio)\n/card show [user_id] - show a card\n/card search <keyword> - search this group's cards\n/card edit [user_id] <key=value>... - edit a card (owner only)",
		KeyCardCreated:           "Card created. Send /card show to view it.",
		KeyCardExists:            "You already have a card. Use /card edit to update it.",
		KeyCardNotFound:          "No card found.",
		KeyCardUpdated:           "Card updated.",
		KeyCardForbidden:         "Only the card owner can edit it.",
		KeyCardAnonymous:         "You must be identified to use card commands; anonymous messages cannot hold a card.",
		KeyCardFieldEmpty:        "Field {field} must not be empty.",
		KeyCardFieldTooLong:      "Field {field} is too long (max {max} characters).",
		KeyCardUnknownField:      "Unknown field {field}; supported: name, title, skills, contact, bio.",
		KeyCardStrayToken:        "Unexpected text {field}; use <key=value> pairs (see /card).",
		KeyCardSearchNone:        "No cards match \"{kw}\".",
		KeyCardSearchMore:        "Showing {shown} of {total} matches; there are more results, please narrow your keyword.",
		KeyCardFailed:            "The card action failed. No change was made.",
		KeyPublishUsage:          "Publish commands:\n/publish text <targets> <text> - send text (targets: comma-separated chat IDs or \"all\")\n/publish photo|video|document <targets> <file_id|URL> [caption] - send media by file_id or URL; local paths are not accepted\n/publish_add - register this chat as a publish target (admins only)\n/publish_remove - unregister this chat (admins only)",
		KeyPublishNoTargets:      "No chat is registered as a publish target. Register one with /publish_add first.",
		KeyPublishTooManyTargets: "Too many targets ({n}); the limit is {max}.",
		KeyPublishTextTooLong:    "The text is too long ({n} characters); the limit is {max}.",
		KeyPublishCaptionTooLong: "The caption is too long ({n} characters); the limit is {max}.",
		KeyPublishBadMediaRef:    "Media must be a file_id or an http(s) URL; local paths are not accepted.",
		KeyPublishUnregistered:   "Chat {id} is not a registered publish target. Register it with /publish_add first.",
		KeyPublishPrivateChat:    "Private chats cannot be publish targets; run /publish_add in the group that should receive publishes.",
		KeyPublishRegistered:     "This chat is registered as a publish target.",
		KeyPublishAlreadyReg:     "This chat is already a publish target.",
		KeyPublishRemoveOk:       "This chat is no longer a publish target.",
		KeyPublishNotRegistered:  "This chat is not a publish target.",
		KeyPublishAllOk:          "Publish succeeded ({ok}/{n}).",
		KeyPublishPartial:        "Publish finished with failures ({ok}/{n}); see the list below.",
		KeyPublishFailed:         "The publish failed before any message was sent. Nothing was delivered.",
		KeyPublishReasonNoRight:  "bot cannot send here",
		KeyPublishReasonLookup:   "permission check failed",
		KeyPublishReasonSend:     "send failed",
		KeyPublishReasonTimeout:  "send timed out",
		KeyPublishNotRecorded:    "Messages were sent, but saving the publish record failed: this send is NOT recorded. It must not be treated as a completed publish; check the storage before re-publishing.",
		KeyPersistFailed:         "Persistence failed: the change was not saved and is not in effect. It would be lost on restart; please make sure the storage is writable and try again.",
		KeyCloneUsage:            "Usage: /clone <source_chat_id> - copy the source chat's bot configuration and rules into this chat. Requires administrator rights in both chats.",
		KeyCloneSame:             "The source chat is this chat; there is nothing to clone.",
		KeyCloneNotAdmin:         "Cloning requires administrator rights in both the source chat and this chat.",
		KeyCloneDone:             "Configuration cloned from chat {src}; this chat's configuration is now revision {version}.",
		KeyTemplateUsage:         "Template commands:\n/template save <name> - save this chat's configuration and rules as a template\n/template apply <name> - apply a template to this chat\n/template export <name> - show a template as versioned JSON\n/template import <name> <JSON> - import a versioned JSON template",
		KeyTemplateNameBad:       "Invalid template name: use 1-32 letters, digits, '_' or '-'.",
		KeyTemplateNotFound:      "No template named {name}.",
		KeyTemplateSaved:         "Template {name} saved (revision {version}).",
		KeyTemplateApplied:       "Template {name} applied; this chat's configuration is now revision {version}.",
		KeyTemplateBadJSON:       "The template payload is not valid JSON.",
		KeyTemplateBadVersion:    "Unknown template version; expected format version 1.",
		KeyTemplateInvalid:       "The template is invalid: {reason}",
		KeyMemberUsage:           "Usage: reply to a member's message with /restrict or /unrestrict, or send /restrict <user_id> or /unrestrict <user_id>.",
		KeyMemberRestricted:      "Member {id} has been restricted.",
		KeyMemberUnrestrict:      "Member {id} is no longer restricted.",
		KeyMemberProtected:       "This action cannot be applied to this member: they are a chat administrator, the chat owner, or their status could not be verified.",
		KeyMemberFailed:          "Could not confirm the result of the member action. Please check member permissions.",
		KeyMemberNotApplicable:   "This action does not apply to banned or non-member users.",
		KeyWelcomeNewMember:      "Welcome, @{name}! Please read the group rules.",
		KeySpamWarning:           "Please slow down, @{name}.",
		KeyBlockedKeyword:        "Your message was removed because it contained a blocked term.",
		KeyNotAdmin:              "Only group administrators may use this command.",
		KeyLanguageSet:           "Language set to English.",
		KeyTimezoneSet:           "Timezone set to {tz}.",
	},
	Chinese: {
		KeyStart:                 "你好！我是群组管理 Bot。发送 /help 查看可用功能。",
		KeyHelp:                  "可用命令：\n/start - 显示帮助\n/help - 显示本消息\n/welcome - 欢迎新成员\n/lang - 设置语言（en, zh）\n/tz - 设置 IANA 时区\n/restrict [user_id] - 限制成员（回复消息或指定用户 ID）\n/unrestrict [user_id] - 解除成员限制（回复消息或指定用户 ID）\n/card - AI 名片：创建、查看、搜索、编辑\n/publish <类型> <目标> <内容> - 向已注册的 chat 发送文本/图片/视频/文档（仅管理员）\n/publish_add - 将本 chat 注册为发布目标（仅管理员）\n/publish_remove - 取消本 chat 的发布目标注册（仅管理员）\n/clone <源chat ID> - 将另一 chat 的配置与规则复制到本 chat（需双方管理员）\n/template - 保存/应用/导出/导入配置模板（仅管理员）",
		KeyCardUsage:             "名片命令：\n/card create <key=value>... - 创建名片（必填：name；可选：title、skills、contact、bio）\n/card show [user_id] - 查看名片\n/card search <keyword> - 在本群搜索名片\n/card edit [user_id] <key=value>... - 编辑名片（仅所有者）",
		KeyCardCreated:           "名片已创建，发送 /card show 查看。",
		KeyCardExists:            "你已拥有名片，使用 /card edit 更新。",
		KeyCardNotFound:          "未找到名片。",
		KeyCardUpdated:           "名片已更新。",
		KeyCardForbidden:         "只有名片所有者才能编辑。",
		KeyCardAnonymous:         "需要确认身份才能使用名片命令；匿名消息无法持有名片。",
		KeyCardFieldEmpty:        "字段 {field} 不能为空。",
		KeyCardFieldTooLong:      "字段 {field} 过长（最多 {max} 个字符）。",
		KeyCardUnknownField:      "未知字段 {field}；支持：name、title、skills、contact、bio。",
		KeyCardStrayToken:        "无法识别的内容 {field}；请使用 <key=value> 形式（参见 /card）。",
		KeyCardSearchNone:        "没有匹配 \"{kw}\" 的名片。",
		KeyCardSearchMore:        "已展示 {shown} 条，共 {total} 条匹配；还有更多结果，请缩小关键字。",
		KeyCardFailed:            "名片操作失败，未做任何更改。",
		KeyPublishUsage:          "发布命令：\n/publish text <目标> <文本> - 发送文本（目标：逗号分隔的 chat ID 或 \"all\"）\n/publish photo|video|document <目标> <file_id|URL> [说明] - 通过 file_id 或 URL 发送媒体；不接受本地路径\n/publish_add - 将本 chat 注册为发布目标（仅管理员）\n/publish_remove - 取消本 chat 的注册（仅管理员）",
		KeyPublishNoTargets:      "尚无已注册的发布目标 chat。请先用 /publish_add 注册。",
		KeyPublishTooManyTargets: "目标过多（{n} 个）；上限为 {max} 个。",
		KeyPublishTextTooLong:    "文本过长（{n} 个字符）；上限为 {max} 个。",
		KeyPublishCaptionTooLong: "说明过长（{n} 个字符）；上限为 {max} 个。",
		KeyPublishBadMediaRef:    "媒体必须是 file_id 或 http(s) URL；不接受本地路径。",
		KeyPublishUnregistered:   "chat {id} 未注册为发布目标。请先用 /publish_add 注册。",
		KeyPublishPrivateChat:    "私聊不能作为发布目标；请在应接收发布内容的群组中发送 /publish_add。",
		KeyPublishRegistered:     "本 chat 已注册为发布目标。",
		KeyPublishAlreadyReg:     "本 chat 已经是发布目标。",
		KeyPublishRemoveOk:       "本 chat 已不再是发布目标。",
		KeyPublishNotRegistered:  "本 chat 不是发布目标。",
		KeyPublishAllOk:          "发布完成（{ok}/{n}）。",
		KeyPublishPartial:        "发布存在失败（{ok}/{n}），详见下方列表。",
		KeyPublishFailed:         "发布在发送任何消息之前失败，未投递任何内容。",
		KeyPublishReasonNoRight:  "Bot 无权在此发送",
		KeyPublishReasonLookup:   "权限校验失败",
		KeyPublishReasonSend:     "发送失败",
		KeyPublishReasonTimeout:  "发送超时",
		KeyPublishNotRecorded:    "消息已发送，但发布记录保存失败：本次发送未被记录，不能视为发布完成。请在确认存储可用后再重发。",
		KeyPersistFailed:         "持久化失败：变更未保存、未生效，重启后会丢失。请确保存储可写后重试。",
		KeyCloneUsage:            "用法：/clone <源chat ID> - 将源 chat 的 Bot 配置与规则复制到本 chat。需要双方 chat 的管理员权限。",
		KeyCloneSame:             "源 chat 就是本 chat，无需克隆。",
		KeyCloneNotAdmin:         "克隆需要源 chat 与本 chat 双方的管理员权限。",
		KeyCloneDone:             "已从 chat {src} 克隆配置；本 chat 的配置现为版本 {version}。",
		KeyTemplateUsage:         "模板命令：\n/template save <名称> - 将本 chat 的配置与规则保存为模板\n/template apply <名称> - 将模板应用到本 chat\n/template export <名称> - 以带版本号的 JSON 展示模板\n/template import <名称> <JSON> - 导入带版本号的 JSON 模板",
		KeyTemplateNameBad:       "模板名称无效：请使用 1-32 个字母、数字、“_”或“-”。",
		KeyTemplateNotFound:      "不存在名为 {name} 的模板。",
		KeyTemplateSaved:         "模板 {name} 已保存（版本 {version}）。",
		KeyTemplateApplied:       "模板 {name} 已应用；本 chat 的配置现为版本 {version}。",
		KeyTemplateBadJSON:       "模板内容不是有效的 JSON。",
		KeyTemplateBadVersion:    "未知的模板版本；期望格式版本 1。",
		KeyTemplateInvalid:       "模板无效：{reason}",
		KeyMemberUsage:           "用法：回复成员的消息并发送 /restrict 或 /unrestrict，或发送 /restrict <user_id> 或 /unrestrict <user_id>。",
		KeyMemberRestricted:      "成员 {id} 已被限制。",
		KeyMemberUnrestrict:      "成员 {id} 已解除限制。",
		KeyMemberProtected:       "无法对该成员执行此操作：其为群管理员、群主，或无法确认其身份状态。",
		KeyMemberFailed:          "无法确认操作结果，请检查成员权限。",
		KeyMemberNotApplicable:   "该操作不适用于被封禁/非成员用户。",
		KeyWelcomeNewMember:      "欢迎 @{name}！请阅读群规。",
		KeySpamWarning:           "@{name}，请放慢发送速度。",
		KeyBlockedKeyword:        "你的消息因包含被禁止的词汇已被移除。",
		KeyNotAdmin:              "只有群管理员才能使用该命令。",
		KeyLanguageSet:           "语言已设置为中文。",
		KeyTimezoneSet:           "时区已设置为 {tz}。",
	},
}

// Catalog returns the message map for a language, falling back to English.
func Catalog(lang Language) map[MessageKey]string {
	if c, ok := catalogs[lang]; ok {
		return c
	}
	return catalogs[English]
}

// T resolves a message key for a language, applying placeholder substitution.
// Every "{key}" in the message is replaced by args[key]; unknown placeholders
// are left untouched.
func T(lang Language, key MessageKey, args map[string]string) string {
	msg := Catalog(lang)[key]
	if msg == "" {
		msg = catalogs[English][key]
	}
	for k, v := range args {
		msg = strings.ReplaceAll(msg, "{"+k+"}", v)
	}
	return msg
}

// Normalize maps a user-supplied language code to a supported Language.
func Normalize(code string) Language {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "zh", "zh-cn", "zh-tw", "chinese", "中文":
		return Chinese
	default:
		return English
	}
}
