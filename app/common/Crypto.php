<?php
/**
 * 凭据加密（可逆）
 *
 * ⚠️ 先分清「哈希」和「加密」—— 这两个词经常被混用，但用途完全相反：
 *
 *   · **哈希**（hash）：单向、不可逆。只能「验证输入是否等于原值」。
 *     适用于**我们自己要校验、但永远不需要读出原文**的东西 —— 典型就是登录密码。
 *     本项目管理员密码用 `password_hash()`（bcrypt）存储，属于这一类。
 *     如果哪天需要「把用户密码解密出来看」，那说明设计已经错了。
 *
 *   · **加密**（encrypt）：双向、可逆。能解出原文。
 *     适用于**程序自己需要拿着原文去用**的东西 —— 典型就是**上游 API Key**：
 *     我们必须在每次转发请求时把它放进 Authorization 头，所以必须能解出原文。
 *     这类数据才需要本文件。
 *
 * 所以：**登录密码不加密（已经哈希了，且这样更安全），上游 Key 才加密。**
 *
 * ═══ 实现说明 ═══
 *
 * 算法选 AES-256-GCM，而不是更常见的 AES-CBC。原因是 GCM 属于
 * 「认证加密（AEAD）」：它除了保密，还自带完整性校验。
 * 用 CBC 的话，攻击者虽然解不出内容，但可以篡改密文，
 * 程序解密后拿到一段被悄悄改过的数据（例如把上游地址改到别人的服务器）却毫无察觉。
 * GCM 会在解密时直接报错，从根上避免这类「密文篡改」攻击。
 *
 * 密钥来源：.env 里的 APP_KEY。
 *
 * ⚠️ **APP_KEY 一旦设定并已加密过数据，就绝不能再改** ——
 *    改了之后旧密文全部无法解密（上游 Key 会集体失效）。
 *    这也是本文件提供 isConfigured() 的原因：没配 APP_KEY 时要尽早报错，
 *    而不是等到用户第一次调用接口时才发现。
 */

declare(strict_types=1);

namespace app\common;

use RuntimeException;

final class Crypto
{
    /** 加密算法：AES-256-GCM（认证加密） */
    private const CIPHER = 'aes-256-gcm';

    /** GCM 的 IV 长度（字节）。每次加密都必须用全新的随机 IV */
    private const IV_LENGTH = 12;

    /** 派生后的 32 字节密钥，进程内缓存 */
    private static ?string $key = null;

    /** 密文前缀，便于将来换算法时做版本识别与平滑迁移 */
    private const PREFIX = 'v1:';

    /**
     * APP_KEY 是否已配置。
     *
     * 后台与启动流程可以用它做前置检查，给出「请先配置 APP_KEY」这种明确提示，
     * 而不是抛一个底层异常让人摸不着头脑。
     */
    public static function isConfigured(): bool
    {
        return trim((string) (getenv('APP_KEY') ?: '')) !== '';
    }

    /**
     * 加密。
     *
     * 输出格式：v1:base64( iv | tag | ciphertext )
     * 把 IV 和认证标签拼在密文前面一起存 —— 它们不是秘密，但解密时必须原样取回。
     */
    public static function encrypt(string $plaintext): string
    {
        $iv = random_bytes(self::IV_LENGTH);
        $tag = '';

        $ciphertext = openssl_encrypt(
            $plaintext,
            self::CIPHER,
            self::key(),
            OPENSSL_RAW_DATA,
            $iv,
            $tag
        );

        if ($ciphertext === false) {
            throw new RuntimeException('加密失败：' . openssl_error_string());
        }

        return self::PREFIX . base64_encode($iv . $tag . $ciphertext);
    }

    /**
     * 解密。
     *
     * 若密文被篡改、APP_KEY 不匹配、或数据格式不对，都会抛异常 —— 这是刻意的：
     * 静默返回空字符串会让「Key 解密失败」表现成「上游返回 401」，
     * 排查时会绕很远的路。
     */
    public static function decrypt(string $payload): string
    {
        if (!str_starts_with($payload, self::PREFIX)) {
            throw new RuntimeException('密文格式无法识别（缺少版本前缀）');
        }

        $raw = base64_decode(substr($payload, strlen(self::PREFIX)), true);
        if ($raw === false || strlen($raw) <= self::IV_LENGTH + 16) {
            throw new RuntimeException('密文内容损坏');
        }

        $iv = substr($raw, 0, self::IV_LENGTH);
        $tag = substr($raw, self::IV_LENGTH, 16);
        $ciphertext = substr($raw, self::IV_LENGTH + 16);

        $plaintext = openssl_decrypt(
            $ciphertext,
            self::CIPHER,
            self::key(),
            OPENSSL_RAW_DATA,
            $iv,
            $tag
        );

        if ($plaintext === false) {
            throw new RuntimeException(
                '解密失败：APP_KEY 与加密时不一致，或密文已被篡改'
            );
        }

        return $plaintext;
    }

    /**
     * 生成一个可用于 APP_KEY 的随机密钥（32 字节十六进制串）。
     * 供安装向导 / 文档提示用户使用，避免用户自己随手敲一个弱密钥。
     */
    public static function generateAppKey(): string
    {
        return bin2hex(random_bytes(32));
    }

    /**
     * 取得派生后的加密密钥（进程内缓存）。
     *
     * 这里不直接用 APP_KEY 字符串，而是先做一次 SHA-256 派生：
     * APP_KEY 是用户手写的，长度和字符集都不可控，
     * 直接当密钥用会削弱算法强度。派生后固定为 32 字节，符合 AES-256 要求。
     */
    private static function key(): string
    {
        if (self::$key !== null) {
            return self::$key;
        }

        $appKey = trim((string) (getenv('APP_KEY') ?: ''));
        if ($appKey === '') {
            throw new RuntimeException(
                'APP_KEY 未配置：请在 .env 中设置 APP_KEY 后再使用加密功能。'
                . '生成方式：php -r "echo bin2hex(random_bytes(32));"'
            );
        }

        return self::$key = hash('sha256', $appKey, true);
    }
}
