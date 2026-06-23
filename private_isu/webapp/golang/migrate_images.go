//go:build ignore

package main

import (
	"fmt"
	"log"
	"os"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

func main() {
	db, err := sqlx.Open("mysql", "isuconp:isuconp@tcp(127.0.0.1:3306)/isuconp?interpolateParams=true")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	rows, err := db.Query("SELECT `id`, `mime`, `imgdata` FROM `posts`")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	outDir := "../public/image"
	os.MkdirAll(outDir, 0755)

	count := 0
	for rows.Next() {
		var id int
		var mime string
		var imgdata []byte

		if err := rows.Scan(&id, &mime, &imgdata); err != nil {
			log.Fatal(err)
		}

		ext := ""
		switch mime {
		case "image/jpeg":
			ext = "jpg"
		case "image/png":
			ext = "png"
		case "image/gif":
			ext = "gif"
		default:
			log.Printf("unknown mime: %s (id=%d)", mime, id)
			continue
		}

		path := fmt.Sprintf("%s/%d.%s", outDir, id, ext)

		// すでにファイルがあればスキップ
		if _, err := os.Stat(path); err == nil {
			continue
		}

		if err := os.WriteFile(path, imgdata, 0644); err != nil {
			log.Fatal(err)
		}
		count++
	}

	fmt.Printf("完了: %d件の画像をファイルに書き出しました\n", count)
}
