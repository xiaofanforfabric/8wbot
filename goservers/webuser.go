package main

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// handleWebuser parses and executes the webuser SSH command.
// Syntax:
//
//	webuser set <uid> level_uid <target_level>
//	webuser set <uid> status ban <reason>
//	webuser set <uid> status ok <reason>
func handleWebuser(db *sql.DB, args string) string {
	parts := strings.Fields(args)
	if len(parts) < 4 {
		return "usage: webuser set <uid> level_uid|status <value> [reason]"
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

	default:
		return "unknown action: use level_uid or status"
	}
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
