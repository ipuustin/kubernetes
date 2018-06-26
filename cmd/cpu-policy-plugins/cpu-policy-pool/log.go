// Copyright 2018 Intel Corporation. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"github.com/golang/glog"
)

//
// errors and logging
//

func logFormat(format string, args ...interface{}) string {
	return fmt.Sprintf(logPrefix+format, args...)
}

func logVerbose(level glog.Level, format string, args ...interface{}) {
	glog.V(level).Infof(logFormat(logPrefix+format, args...))
}

func logInfo(format string, args ...interface{}) {
	glog.Info(logFormat(format, args...))
}

func logWarning(format string, args ...interface{}) {
	glog.Warningf(logFormat(format, args...))
}

func logError(format string, args ...interface{}) {
	glog.Errorf(logFormat(format, args...))
}

func logFatal(format string, args ...interface{}) {
	glog.Fatalf(logFormat(format, args...))
}

func logPanic(format string, args ...interface{}) {
	logFatal(format, args...)
	panic(logFormat(format, args...))
}
