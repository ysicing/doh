# doh

> 简单的 DNS over HTTPS (DoH) 实现

## 简介

这是一个轻量级的 DNS over HTTPS 工具，可以通过 HTTPS 协议进行安全的 DNS 查询。

## 功能特点

- 支持 DNS over HTTPS 协议
- 简单易用的接口
- 安全的 DNS 查询

## 使用

```bash
q www.google.com @http://127.0.0.1:65001 | nali
www.google.com [Google Web 业务] . 5m A 199.16.158.9 [美国 Twitter公司]
www.google.com [Google Web 业务] . 5m A 31.13.106.4 [瑞典]
www.google.com [Google Web 业务] . 5m A 199.16.158.182 [美国 Twitter公司]
www.google.com [Google Web 业务] . 5m A 199.59.148.229 [美国 Twitter公司]
www.google.com [Google Web 业务] . 5m AAAA 2001::1 [IANA特殊地址 Teredo隧道地址]
www.google.com [Google Web 业务] . 5m A 31.13.94.41 [爱尔兰 Facebook分公司]
```
