/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package glusterfs

import (
	"bufio"
	"fmt"
	"os"

	"k8s.io/klog/v2"
)

// readGlusterLog will take the last 2 lines of the log file
// on failure of gluster SetUp and return those so kubelet can
// properly expose them
// return error on any failure
// readGlusterlog 当配置gluster时发生错误，将日志文件的最后2行返回，以kubelet可以适当暴露在任何失败返回错误
func readGlusterLog(path string, podName string) error {

	var line1 string
	var line2 string
	linecount := 0

	klog.Infof("failure, now attempting to read the gluster log for pod %s", podName)

	// Check and make sure path exists
	// 检测路径是否存在
	if len(path) == 0 {
		return fmt.Errorf("log file does not exist for pod %s", podName)
	}

	// open the log file
	// 打开日志文件
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("could not open log file for pod %s", podName)
	}
	defer file.Close()

	// read in and scan the file using scanner
	// from stdlib
	// 从stdlib使用scanner扫描文件并读入
	fscan := bufio.NewScanner(file)

	// rather than guessing on bytes or using Seek
	// going to scan entire file and take the last two lines
	// generally the file should be small since it is pod specific
	// 由于是pod spec,所以一般的文件应该小,将扫描整个文件,读取最后两行,而不是限制字节
	for fscan.Scan() {
		if linecount > 0 {
			line1 = line2
		}
		line2 = "\n" + fscan.Text()

		linecount++
	}

	if linecount > 0 {
		return fmt.Errorf("%v", line1+line2+"\n")
	}
	return nil
}
