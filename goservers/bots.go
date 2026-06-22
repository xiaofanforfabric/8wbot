package main

import "database/sql"

// Bot CRUD functions

func createBot(db *sql.DB, b *BotData) error {
	_, err := db.Exec(`INSERT INTO bots(belong, creation_time, username, dsl, status) VALUES(?,?,?,?,?)`,
		b.Belong, b.CreationTime, b.Username, boolToInt(b.DSL), b.Status)
	return err
}

func getBotsByUser(db *sql.DB, belong string) ([]BotData, error) {
	rows, err := db.Query("SELECT belong, creation_time, username, dsl, COALESCE(status,'no') FROM bots WHERE belong = ? ORDER BY id", belong)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var bots []BotData
	for rows.Next() {
		var b BotData
		var dslInt int64
		var statusStr string
		if err := rows.Scan(&b.Belong, &b.CreationTime, &b.Username, &dslInt, &statusStr); err != nil {
			return nil, err
		}
		b.DSL = dslInt != 0
		b.Status = statusStr
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
	row := db.QueryRow("SELECT belong, creation_time, username, dsl, COALESCE(status,'no') FROM bots WHERE username = ?", username)
	var b BotData
	var dslInt int64
	err := row.Scan(&b.Belong, &b.CreationTime, &b.Username, &dslInt, &b.Status)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b.DSL = dslInt != 0
	return &b, nil
}

// updateBotOwner reassigns a bot to a new owner (used when username exists with status 'no')
func updateBotOwner(db *sql.DB, username string, newBelong string) error {
	_, err := db.Exec("UPDATE bots SET belong = ? WHERE username = ?", newBelong, username)
	return err
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
