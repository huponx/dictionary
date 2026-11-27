package service

import (
	"testing"

	"github.com/huponx/gocommon/grpcserver"
	dictionaryv1 "github.com/huponx/protogen/go/dictionary/v1"
)

// Every RPC of DictionaryService has exactly one rule and every name in them is declared in permissions.proto.
func TestCoverage(t *testing.T) {
	grpcserver.CheckCoverage(t, dictionaryv1.File_dictionary_v1_dictionary_proto, dictionaryv1.File_dictionary_v1_permissions_proto)
	for _, m := range dictionaryv1.DictionaryService_ServiceDesc.Methods {
		full := "/" + dictionaryv1.DictionaryService_ServiceDesc.ServiceName + "/" + m.MethodName
		if _, ok := Permissions[full]; !ok {
			t.Errorf("%s is not in Permissions", full)
		}
	}
	for _, s := range dictionaryv1.DictionaryService_ServiceDesc.Streams {
		full := "/" + dictionaryv1.DictionaryService_ServiceDesc.ServiceName + "/" + s.StreamName
		if _, ok := Permissions[full]; !ok {
			t.Errorf("%s is not in Permissions", full)
		}
	}
}

func TestPermissionsOfTheMethods(t *testing.T) {
	cases := map[string]string{
		"LookupWord": "dictionary:word:read", "SaveWord": "dictionary:word:write",
		"GetSettings": "dictionary:settings:read", "UpdateSettings": "dictionary:settings:write",
		"SubmitReview": "dictionary:word:read",
	}
	for m, want := range cases {
		r := Permissions["/dictionary.v1.DictionaryService/"+m]
		if len(r.Any) != 1 || r.Any[0] != want || len(r.All) != 0 || r.Public {
			t.Errorf("%s: %+v, want %s", m, r, want)
		}
	}
	for _, m := range []string{"DownloadDeck", "DownloadExportPart"} {
		if !Permissions["/dictionary.v1.DictionaryService/"+m].Public {
			t.Errorf("%s must be public", m)
		}
	}
}
