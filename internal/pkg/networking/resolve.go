// Copyright Red Hat, Inc.
//
// Licensed under the Apache License, Version 2.0 (the License);
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an AS IS BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package networking

import (
	"fmt"
	"net"

	"istio.io/istio/pkg/slices"
)

func Resolve(addrs ...string) []string {
	var allIPs []string
	for _, addr := range addrs {
		if IsIP(addr) {
			allIPs = append(allIPs, addr)
			continue
		}

		ips, err := net.LookupIP(addr)
		if err != nil {
			fmt.Printf("Failed to resolve '%s': %v\n", addr, err)
		}
		stringIPs := slices.Map(ips, func(ip net.IP) string {
			return ip.String()
		})
		allIPs = append(allIPs, stringIPs...)
	}
	return allIPs
}

func IsIP(s string) bool {
	return net.ParseIP(s) != nil
}
