// Copyright (C) 2019-2026 Chrystian Huot <chrystian.huot@saubeo.solutions>
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>

package upload

import (
	"errors"
	"testing"

	"rdio-scanner/internal/domain"
)

// SDRTrunk's connection test names no talkgroup and carries no audio; it is marked connected
// only by the answer 6.6.3 gave it, "no talkgroup".
func TestAnUploadNamingNoTalkgroupIsAnsweredNoTalkgroupAsVersion663Answered(t *testing.T) {
	call := domain.NewCall()

	if got := incompleteReason(call, errors.New("no audio")); got != "no talkgroup" {
		t.Errorf("incompleteReason = %q, want %q", got, "no talkgroup")
	}
}

func TestAnUploadNamingATalkgroupKeepsItsOwnReason(t *testing.T) {
	call := domain.NewCall()
	call.Meta.TalkgroupRef = 1514000

	if got := incompleteReason(call, errors.New("no audio")); got != "no audio" {
		t.Errorf("incompleteReason = %q, want %q", got, "no audio")
	}
}
