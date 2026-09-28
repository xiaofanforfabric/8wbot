package main

import (
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"
)

// handleWebuser parses and executes the webuser SSH command.
// Syntax:
//
//	webuser set <uid> level_uid <target_level>
//	webuser set <uid> status ban <reason>
//	webuser set <uid> status ok <reason>
//	webuser set <uid> quota <remaining_bots>
func handleWebuser(db *sql.DB, args string) string {
	parts := strings.Fields(args)
	if len(parts) < 4 {
		return "usage: webuser set <uid> level_uid|status|quota <value> [reason]"
	}
	if parts[0] != "set" {
		return "usage: webuser set ..."
	}

	uid := parts[1]
	action := parts[2]

	switch action {
	case "level_uid":
		if len(parts) < 4 {
			return "usage: webuser set <uid> level_uid <target_level>"
		}
		targetLevel, err := strconv.ParseInt(parts[3], 10, 64)
		if err != nil {
			return "invalid level_uid: must be an integer"
		}
		return setUserLevel(db, uid, targetLevel)

	case "status":
		if len(parts) < 4 {
			return "usage: webuser set <uid> status ban|ok <reason>"
		}
		status := parts[3]
		if status != "ban" && status != "ok" {
			return "status must be 'ban' or 'ok'"
		}
		reason := ""
		if len(parts) > 4 {
			reason = strings.Join(parts[4:], " ")
		}
		return setUserStatus(db, uid, status, reason)

	case "quota":
		if len(parts) < 4 {
			return "usage: webuser set <uid> quota <remaining_bots>"
		}
		n, err := strconv.ParseInt(parts[3], 10, 64)
		if err != nil {
			return "invalid quota: must be an integer"
		}
		if n < 0 {
			return "invalid quota: must be >= 0"
		}
		return setUserQuota(db, uid, n)

	default:
		return "unknown action: use level_uid, status or quota"
	}
}

func setUserQuota(db *sql.DB, uid string, quota int64) string {
	u, err := findUser(db, uid)
	if err != nil {
		return "db error: " + err.Error()
	}
	if u == nil {
		return "user not found: " + uid
	}

	old := u.RemainingBotCreationQuantity
	u.RemainingBotCreationQuantity = quota
	if err := upsertUser(db, u); err != nil {
		return "db error: " + err.Error()
	}

	return fmt.Sprintf("ok: user %s quota changed %d → %d", uid, old, quota)
}

// loadBannedUsers 启动时把库里 status='ban' 的用户读回内存。
//
// 少了这一步，ban 只在「本次进程内」有效：bannedUsers 是内存 map，
// 而唯一的执法点（/api/userdata）只查这张 map。进程一重启 map 就空了，
// 库里还写着 ban 但没有任何代码去查它，被封的人自动放行。
//
// 注意这里只读 status 字段，不看 level_id —— level_uid 目前只是展示用，
// 不参与任何鉴权判断。
func loadBannedUsers(db *sql.DB) {
	if db == nil {
		return
	}
	rows, err := db.Query("SELECT jht_uid FROM userdata WHERE status = 'ban'")
	if err != nil {
		log.Printf("[BAN] 加载封禁名单失败: %v", err)
		return
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			continue
		}
		if uid != "" {
			bannedUsers.Store(uid, true)
			n++
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("[BAN] 遍历封禁名单失败: %v", err)
		return
	}
	log.Printf("[BAN] 已从数据库加载 %d 个封禁用户", n)
}

func setUserLevel(db *sql.DB, uid string, targetLevel int64) string {
	u, err := findUser(db, uid)
	if err != nil {
		return "db error: " + err.Error()
	}
	if u == nil {
		return "user not found: " + uid
	}

	oldLevel := u.LevelID
	u.LevelID = targetLevel
	if err := upsertUser(db, u); err != nil {
		return "db error: " + err.Error()
	}

	var levelName string
	switch {
	case targetLevel == 0:
		levelName = "root"
	case targetLevel <= 1000:
		levelName = "system"
	case targetLevel <= 2000:
		levelName = "ssh"
	default:
		levelName = "user"
	}

	return fmt.Sprintf("ok: user %s level changed %d (%s) → %d (%s)", uid, oldLevel, levelNameFromID(oldLevel), targetLevel, levelName)
}

func setUserStatus(db *sql.DB, uid string, status string, reason string) string {
	u, err := findUser(db, uid)
	if err != nil {
		return "db error: " + err.Error()
	}
	if u == nil {
		return "user not found: " + uid
	}

	u.Status = status
	u.StatusInfo = reason
	if err := upsertUser(db, u); err != nil {
		return "db error: " + err.Error()
	}

	if status == "ban" {
		// Also add to bannedUsers map for fast API check
		bannedUsers.Store(uid, true)
		return fmt.Sprintf("ok: user %s banned — %s", uid, reason)
	}
	// unban
	bannedUsers.Delete(uid)
	return fmt.Sprintf("ok: user %s unbanned — %s", uid, reason)
}

func levelNameFromID(id int64) string {
	switch {
	case id == 0:
		return "root"
	case id <= 1000:
		return "system"
	case id <= 2000:
		return "ssh"
	default:
		return "user"
	}
}
