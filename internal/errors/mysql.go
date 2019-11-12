//
// Copyright (C) 2019 kpango (Yusuke Kato)
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

// Package errors provides error types and function
package errors

// "github.com/pkg/errors"

var (
	// MySQL
	ErrMySQLConnectionClosed = New("MySQL connection closed")

	ErrRequiredElementNotFoundByUUID = func(uuid string) error {
		return Errorf("Required element not found, uuid: %s", uuid)
	}

	ErrRequiredMemberNotFilled = func(member string) error {
		return Errorf("Required member not filled (member: %s)", member)
	}
)