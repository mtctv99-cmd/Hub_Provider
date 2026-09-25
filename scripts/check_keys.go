package main

import (
	"database/sql"
	"fmt"
	_ "modernc.org/sqlite"
)

func main() {
	db, err := sql.Open("sqlite", "data/orchestrator.db")
	if err != nil {
		panic(err)
	}
	defer db.Close()

	rows, err := db.Query("SELECT id, name, key_hash, scopes, status FROM api_keys")
	if err != nil {
		panic(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		var id, name, keyHash, scopes, status string
		rows.Scan(&id, &name, &keyHash, &scopes, &status)
		fmt.Printf("KEY: id=%s name=%s scopes=%s status=%s\n", id, name, scopes, status)
	}
	fmt.Printf("Total keys: %d\n", count)
}
