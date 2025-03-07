// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/lib/pq"
	"golang.org/x/pkgsite/internal"
	"golang.org/x/pkgsite/internal/database"
	"golang.org/x/pkgsite/internal/derrors"
	"golang.org/x/pkgsite/internal/experiment"
)

func upsertSymbolSearchDocuments(ctx context.Context, tx *database.DB,
	modulePath, v string, unitIDs []int) (err error) {
	defer derrors.Wrap(&err, "upsertSymbolSearchDocuments(ctx, ddb, %q, %q)", modulePath, v)

	if !experiment.IsActive(ctx, internal.ExperimentInsertSymbolSearchDocuments) {
		return nil
	}

	// If a user is looking for the symbol "DB.Begin", from package
	// database/sql, we want them to be able to find this by searching for
	// "DB.Begin" and "sql.DB.Begin". Searching for "sql.DB", "DB", "Begin" or
	// "sql.DB" will not return "DB.Begin".
	// If a user is looking for the symbol "DB.Begin", from package
	// database/sql, we want them to be able to find this by searching for
	// "DB.Begin", "Begin", and "sql.DB.Begin". Searching for "sql.DB" or
	// "DB" will not return "DB.Begin".
	q := `
		INSERT INTO symbol_search_documents
		(package_path_id, symbol_name_id, unit_id, tsv_symbol_tokens)
			SELECT
				u.path_id,
				s.id,
				u.id,` +
		// Index <package>.<identifier> (i.e. "sql.DB.Begin")
		`SETWEIGHT( TO_TSVECTOR('simple', concat(s.name, ' ', concat(u.name, '.', s.name))), 'A') ||` +
		// Index <identifier>, including the parent name (i.e. DB.Begin).
		`SETWEIGHT( TO_TSVECTOR('simple', s.name), 'A') ||` +
		// Index <identifier> without parent name (i.e. "Begin").
		//
		// This is weighted less, so that if other symbols are just named
		// "Begin" they will rank higher in a search for "Begin".
		`SETWEIGHT( TO_TSVECTOR('simple', split_part(s.name, '.', 2)), 'B') AS tokens` +
		`
			FROM symbol_names s
			INNER JOIN package_symbols ps ON s.id = ps.symbol_name_id
			INNER JOIN documentation_symbols ds ON ps.id = ds.package_symbol_id
			INNER JOIN documentation d ON d.id = ds.documentation_id
			INNER JOIN units u ON u.id = d.unit_id
			WHERE u.id = ANY($1)
			-- We will get a row for every unit/symbol/goos/goarch, but we only
			-- care about the unit/symbol.
			GROUP BY s.id, u.id, u.path_id
		ON CONFLICT (package_path_id, symbol_name_id)
		DO UPDATE SET
			unit_id=excluded.unit_id,
			tsv_symbol_tokens=excluded.tsv_symbol_tokens`
	_, err = tx.Exec(ctx, q, pq.Array(unitIDs))
	return err
}

// symbolSearch searches all symbols in the symbol_search_documents table for
// the query.
//
// TODO(https://golang.org/issue/44142): factor out common code between
// symbolSearch and deepSearch.
func (db *DB) symbolSearch(ctx context.Context, q string, limit, offset, maxResultCount int) searchResponse {
	query := fmt.Sprintf(`
		SELECT
			package_path,
			version,
			module_path,
			commit_time,
			imported_by_count,
			symbol_name,
		    type,
		    synopsis,
		    goos,
		    goarch,
			COUNT(*) OVER() AS total
		FROM (
			SELECT
				DISTINCT ON (s.name) s.name AS symbol_name,
				sd.package_path,
				sd.version,
				sd.module_path,
				sd.commit_time,
				sd.imported_by_count,
				ps.type,
				ps.synopsis,
				d.goos,
				d.goarch,
				(%s) AS score
			FROM search_documents sd
			INNER JOIN symbol_search_documents ssd ON sd.package_path_id = ssd.package_path_id
			INNER JOIN symbol_names s ON s.id = ssd.symbol_name_id
			INNER JOIN units u ON u.id = ssd.unit_id
			INNER JOIN documentation d ON d.unit_id = u.id
			INNER JOIN documentation_symbols ds ON ds.documentation_id = d.id
			INNER JOIN package_symbols ps ON ps.id = ds.package_symbol_id
			WHERE
				ssd.tsv_symbol_tokens @@ to_tsquery('simple', $1)
			ORDER BY
				symbol_name,
				CASE WHEN goos = 'all' THEN 0
					 WHEN goos = 'linux' THEN 1
					 WHEN goos = 'windows' THEN 2
					 WHEN goos = 'darwin' THEN 3
					 WHEN goos = 'js' THEN 4
					 END
		) r
		WHERE r.score > 0.1
		ORDER BY
			score DESC,
			commit_time DESC,
			symbol_name,
			package_path
		LIMIT $2
		OFFSET $3`, symbolScoreExpr)

	var results []*internal.SearchResult
	collect := func(rows *sql.Rows) error {
		var r internal.SearchResult
		if err := rows.Scan(
			&r.PackagePath,
			&r.Version,
			&r.ModulePath,
			&r.CommitTime,
			&r.NumImportedBy,
			&r.SymbolName,
			&r.SymbolKind,
			&r.SymbolSynopsis,
			&r.SymbolGOOS,
			&r.SymbolGOARCH,
			&r.NumResults); err != nil {
			return fmt.Errorf("symbolSearch: rows.Scan(): %v", err)
		}
		results = append(results, &r)
		return nil
	}

	// Search for an OR of the terms, so that if the user searches for
	// "db begin", queries matching "db" and "begin" will be returned.
	q = strings.Join(strings.Split(q, " "), " | ")
	err := db.db.RunQuery(ctx, query, collect, q, limit, offset)
	if err != nil {
		results = nil
	}
	if len(results) > 0 && results[0].NumResults > uint64(maxResultCount) {
		for _, r := range results {
			r.NumResults = uint64(maxResultCount)
		}
	}
	return searchResponse{
		source:  "symbol",
		results: results,
		err:     err,
	}
}

var symbolScoreExpr = fmt.Sprintf(`
		ts_rank('{0.1, 0.2, 1.0, 1.0}', ssd.tsv_symbol_tokens, to_tsquery('simple', $1)) *
		ln(exp(1)+imported_by_count) *
		CASE WHEN u.redistributable THEN 1 ELSE %f END *
		CASE WHEN COALESCE(has_go_mod, true) THEN 1 ELSE %f END
	`, nonRedistributablePenalty, noGoModPenalty)
