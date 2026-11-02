// Package tacdb is an embeddable graph database with full-text search,
// optional vector search, transactions and durable event streams.
//
// Section references such as §6.1 point to the tacdb specification,
// version 1.3. This package exposes the error API and the graph API of
// §6: handles, transactions, nodes, edges, labels, properties and edge
// cursors. The query language and indexes arrive with later milestones.
//
//	db, err := tacdb.OpenMemory(tacdb.DefaultOptions())
//	if err != nil {
//		return err
//	}
//	defer db.Close()
//	err = db.Update(ctx, func(tx *tacdb.Tx) error {
//		a, err := tx.CreateNode(tacdb.CreateNodeOptions{Labels: []string{"Person"}})
//		if err != nil {
//			return err
//		}
//		b, err := tx.CreateNode(tacdb.CreateNodeOptions{Labels: []string{"Person"}})
//		if err != nil {
//			return err
//		}
//		_, err = tx.CreateEdge(a.ID, b.ID, "KNOWS", nil)
//		return err
//	})
package tacdb
