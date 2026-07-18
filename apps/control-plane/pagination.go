package controlplane

import (
	"net/http"

	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/db"
)

// decodePageParams turns the contract's optional cursor/pageSize query
// parameters (api/openapi.yaml components.parameters.Cursor/PageSize) into
// a store-level db.Cursor and an effective page size. A nil cursorParam
// selects the first page. A present-but-malformed cursor is reported via
// the third return value being false, so the caller can respond with 400
// before ever reaching the store. An absent pageSizeParam defaults to
// db.DefaultPageSize; every size is passed through db.ClampPageSize, which
// mirrors the contract's declared minimum/maximum (1/100).
func decodePageParams(cursorParam *contracts.Cursor, pageSizeParam *contracts.PageSize) (cursor *db.Cursor, pageSize int, ok bool) {
	pageSize = db.DefaultPageSize
	if pageSizeParam != nil {
		pageSize = *pageSizeParam
	}
	pageSize = db.ClampPageSize(pageSize)
	if cursorParam == nil {
		return nil, pageSize, true
	}
	decoded, err := db.DecodeCursor(*cursorParam)
	if err != nil {
		return nil, pageSize, false
	}
	return &decoded, pageSize, true
}

// writeInvalidCursor reports a cursor that decodePageParams could not
// decode, per the ErrorResponse shape every endpoint in api/openapi.yaml
// shares.
func writeInvalidCursor(response http.ResponseWriter, request *http.Request) {
	writeError(response, request, http.StatusBadRequest, "INVALID_CURSOR", "The pagination cursor is not valid.")
}
