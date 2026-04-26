// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package export

func string2Map(a, b []string) map[string]string {
	a2b := make(map[string]string, len(a))
	for i, str := range a {
		a2b[str] = b[i]
	}
	return a2b
}

// needRepeatableRead reports whether dumping connections need REPEATABLE READ.
// True for PostgreSQL snapshot consistency.
func needRepeatableRead(consistency string) bool {
	return consistency == ConsistencyTypeSnapshot
}

func infiniteChan[T any]() (chan<- T, <-chan T) {
	in, out := make(chan T), make(chan T)

	go func() {
		var (
			q  []T
			e  T
			ok bool
		)
		handleRead := func() bool {
			if !ok {
				for _, e = range q {
					out <- e
				}
				close(out)
				return true
			}
			q = append(q, e)
			return false
		}
		for {
			if len(q) > 0 {
				select {
				case e, ok = <-in:
					if handleRead() {
						return
					}
				case out <- q[0]:
					q = q[1:]
				}
			} else {
				e, ok = <-in
				if handleRead() {
					return
				}
			}
		}
	}()
	return in, out
}
