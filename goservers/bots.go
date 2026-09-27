package main

import "database/sql"

// Bot CRUD functions

func createBot(db *sql.DB, b *BotData) error {
	_, err := db.Exec(`INSERT INTO bots(belong, creation_time, username, dsl, status, auto_restore, auto_reconnect) VALUES(?,?,?,?,?,?,?)`,
		b.Belong, b.CreationTime, b.Username, boolToInt(b.DSL), b.Status, boolToInt(b.AutoRestore), boolToInt(b.AutoReconnect))
	return err
}

func getBotsByUser(db *sql.DB, belong string) ([]BotData, error) {
	rows, err := db.Query("SELECT belong, creation_time, username, dsl, COALESCE(status,'no'), COALESCE(auto_restore,1), COALESCE(auto_reconnect,1), COALESCE(last_exit_reason,''), COALESCE(last_exit_type,''), COALESCE(last_exit_time,'') FROM bots WHERE belong = ? ORDER BY id", belong)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var bots []BotData
	for rows.Next() {
		var b BotData
		var dslInt int64
		var statusStr string
		var autoRestoreInt int64
		var autoReconnectInt int64
		if err := rows.Scan(&b.Belong, &b.CreationTime, &b.Username, &dslInt, &statusStr, &autoRestoreInt, &autoReconnectInt,
			&b.LastExitReason, &b.LastExitType, &b.LastExitTime); err != nil {
			return nil, err
		}
		b.DSL = dslInt != 0
		b.Status = statusStr
		b.AutoRestore = autoRestoreInt != 0
		b.AutoReconnect = autoReconnectInt != 0
		bots = append(bots, b)
	}
	return bots, nil
}

func countBotsByUser(db *sql.DB, belong string) (int, error) {
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM bots WHERE belong = ?", belong).Scan(&count)
	return count, err
}

func deleteBot(db *sql.DB, belong string, username string) error {
	_, err := db.Exec("DELETE FROM bots WHERE belong = ? AND username = ?", belong, username)
	return err
}

func setBotDSL(db *sql.DB, belong string, username string, enabled bool) error {
	_, err := db.Exec("UPDATE bots SET dsl = ? WHERE belong = ? AND username = ?", boolToInt(enabled), belong, username)
	return err
}

// findBotByUsername returns the bot and its status, or nil if not found.
func findBotByUsername(db *sql.DB, username string) (*BotData, error) {
	row := db.QueryRow("SELECT belong, creation_time, username, dsl, COALESCE(status,'no'), COALESCE(auto_restore,1), COALESCE(auto_reconnect,1) FROM bots WHERE username = ?", username)
	var b BotData
	var dslInt int64
	var autoRestoreInt int64
	var autoReconnectInt int64
	err := row.Scan(&b.Belong, &b.CreationTime, &b.Username, &dslInt, &b.Status, &autoRestoreInt, &autoReconnectInt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b.DSL = dslInt != 0
	b.AutoRestore = autoRestoreInt != 0
	b.AutoReconnect = autoReconnectInt != 0
	return &b, nil
}

// updateBotOwner reassigns a bot to a new owner (used when username exists with status 'no')
func updateBotOwner(db *sql.DB, username string, newBelong string) error {
	_, err := db.Exec("UPDATE bots SET belong = ? WHERE username = ?", newBelong, username)
	return err
}

// updateBotStatus changes a bot's status, scoped to its owner.
// 必须带上 belong 条件: 归属校验与写入之间存在竞态窗口，
// 若在此期间机器人被他人重新认领，无条件更新会误改他人的机器人。
// 返回受影响行数，调用方可用它判断是否真的改中（0 表示归属已变更）。
func updateBotStatus(db *sql.DB, username string, belong string, status string) (int64, error) {
	res, err := db.Exec("UPDATE bots SET status = ? WHERE username = ? AND belong = ?", status, username, belong)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// setBotAutoReconnect updates the auto_reconnect flag for a bot.
func setBotAutoReconnect(db *sql.DB, username string, enabled bool) error {
	_, err := db.Exec("UPDATE bots SET auto_reconnect = ? WHERE username = ?", boolToInt(enabled), username)
	return err
}

// setBotAutoRestore updates the auto_restore flag for a bot.
func setBotAutoRestore(db *sql.DB, username string, enabled bool) error {
	_, err := db.Exec("UPDATE bots SET auto_restore = ? WHERE username = ?", boolToInt(enabled), username)
	return err
}

// setBotManualStop 标记「这个机器人是用户主动下线的」。
//
// 全局自动重连巡护必须靠它区分两种离线状态，否则会出事：
//   manual_stop = 0 —— 意外掉线（被服务器踢、网络抖动），应该自动拉起来
//   manual_stop = 1 —— 用户自己点了「下线」，必须保持关闭
//
// 少了这个标记，巡护会在用户下线后立刻把机器人重新拉起来，下线按钮等于失效。
func setBotManualStop(db *sql.DB, username string, stopped bool) error {
	_, err := db.Exec("UPDATE bots SET manual_stop = ? WHERE username = ?", boolToInt(stopped), username)
	return err
}

// autoReconnectCandidates 返回需要自动重连巡护的机器人名单：
// 开了自动重连、已完成归属验证、并且不是用户主动下线的。
func autoReconnectCandidates(db *sql.DB) ([]string, error) {
	rows, err := db.Query("SELECT username FROM bots WHERE auto_reconnect = 1 AND status = 'confirmed' AND COALESCE(manual_stop,0) = 0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err == nil {
			names = append(names, name)
		}
	}
	return names, nil
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
