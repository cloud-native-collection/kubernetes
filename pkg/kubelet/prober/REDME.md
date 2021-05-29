检测kubernetes集群中pod的健康状态，支持两种：
- livenessProber: 用来检测容器中的程序是否健康并确定何时重启容器；
- readinessProber: 用来检测容器中的程序是否就绪，就绪意味着该容器可以接收处理流量。
支持HTTP, TCP, EXEC(执行命令或脚本)三种检测方式