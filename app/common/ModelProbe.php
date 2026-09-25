<?php
/**
 * 渠道模型逐项探测内核。
 */

declare(strict_types=1);

namespace app\common;

final class ModelProbe
{
    public const OK = 'ok';
    public const NO_ACCESS = 'no_access';
    public const UNROUTABLE = 'unroutable';
    public const INCONCLUSIVE = 'inconclusive';
    public const AUTH_ERROR = 'auth_error';

    private const KEEP_BYTES = 4096;

    /**
     * @param array{http:int,body:string,curl_error:bool} $response
     */
    public static function classify(array $response): string
    {
        if ($response['curl_error'] || $response['http'] === 0) {
            return self::INCONCLUSIVE;
        }

        $http = $response['http'];
        if ($http === 200) {
            return self::OK;
        }
        if ($http === 401 || $http === 403) {
            return self::AUTH_ERROR;
        }
        if ($http === 404) {
            $body = $response['body'];
            $permission = str_contains($body, '"detail"')
                || stripos($body, 'for account') !== false
                || stripos($body, 'Function') !== false;

            return $permission ? self::NO_ACCESS : self::UNROUTABLE;
        }

        return self::INCONCLUSIVE;
    }

    /**
     * @param array<string, mixed> $channel
     * @param callable|null $sender 测试注入点，签名 fn(array $spec): array
     * @return array{model:string,upstream_model:string,result:string,http:int,summary:string}
     */
    public static function probe(
        array $channel,
        string $key,
        string $model,
        int $timeout = 20,
        ?callable $sender = null
    ): array {
        $upstreamModel = self::upstreamModel($channel, $model);
        $spec = self::buildSpec($channel, $key, $upstreamModel, $timeout);
        $sender ??= [self::class, 'send'];

        $response = $sender($spec);
        $classification = self::classify($response);

        if (self::retryable($response)) {
            usleep(800_000);
            $response = $sender($spec);
            $classification = self::classify($response);
        }

        return [
            'model' => $model,
            'upstream_model' => $upstreamModel,
            'result' => $classification,
            'http' => (int) $response['http'],
            'summary' => self::summary((string) $response['body'], $key),
        ];
    }

    /**
     * @param array<string, mixed> $channel
     * @return array{url:string,headers:array<int,string>,body:string,connect_timeout:int,total_timeout:int,proxy:string,key:string}
     */
    public static function buildSpec(array $channel, string $key, string $upstreamModel, int $timeout): array
    {
        $config = self::config($channel);
        $url = rtrim((string) ($channel['base_url'] ?? ''), '/') . '/chat/completions';
        $extraQuery = (array) ($config['extra_query'] ?? []);
        if ($extraQuery !== []) {
            $url .= '?' . http_build_query($extraQuery);
        }

        $headers = ['Accept: application/json', 'Expect:', 'Content-Type: application/json'];
        $authType = (string) ($config['auth_type'] ?? 'bearer');
        $authName = trim((string) ($config['auth_name'] ?? ''));
        $authPrefix = array_key_exists('auth_prefix', $config)
            ? (string) $config['auth_prefix']
            : ($authType === 'bearer' ? 'Bearer ' : '');

        if ($key !== '') {
            if ($authType === 'bearer') {
                $headers[] = ($authName !== '' ? $authName : 'Authorization') . ': ' . $authPrefix . $key;
            } elseif ($authType === 'header') {
                $headers[] = ($authName !== '' ? $authName : 'api-key') . ': ' . $authPrefix . $key;
            } elseif ($authType === 'query') {
                $url .= (str_contains($url, '?') ? '&' : '?')
                    . rawurlencode($authName !== '' ? $authName : 'key') . '=' . rawurlencode($key);
            }
        }

        foreach ((array) ($config['extra_headers'] ?? []) as $name => $value) {
            $headers[] = is_int($name) ? (string) $value : $name . ': ' . $value;
        }

        $body = array_merge((array) ($config['extra_body'] ?? []), [
            'model' => $upstreamModel,
            'messages' => [['role' => 'user', 'content' => 'hi']],
            'max_tokens' => 1,
            'stream' => false,
        ]);
        foreach ((array) ($config['strip_body'] ?? []) as $field) {
            unset($body[(string) $field]);
        }

        return [
            'url' => $url,
            'headers' => $headers,
            'body' => (string) json_encode($body, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES),
            'connect_timeout' => min(10, max(1, $timeout)),
            'total_timeout' => max(5, $timeout),
            'proxy' => trim((string) ($config['proxy'] ?? '')),
            'key' => $key,
        ];
    }

    /**
     * @param array{url:string,headers:array<int,string>,body:string,connect_timeout:int,total_timeout:int,proxy:string,key:string} $spec
     * @return array{http:int,body:string,curl_error:bool}
     */
    public static function send(array $spec): array
    {
        $ch = curl_init($spec['url']);
        $options = [
            CURLOPT_POST => true,
            CURLOPT_POSTFIELDS => $spec['body'],
            CURLOPT_HTTPHEADER => $spec['headers'],
            CURLOPT_RETURNTRANSFER => true,
            CURLOPT_CONNECTTIMEOUT => $spec['connect_timeout'],
            CURLOPT_TIMEOUT => $spec['total_timeout'],
            CURLOPT_SSL_VERIFYPEER => true,
            CURLOPT_SSL_VERIFYHOST => 2,
        ];
        if ($spec['proxy'] !== '') {
            $options[CURLOPT_PROXY] = $spec['proxy'];
        }
        curl_setopt_array($ch, $options);

        $body = curl_exec($ch);
        $error = curl_error($ch);
        $http = (int) curl_getinfo($ch, CURLINFO_HTTP_CODE);
        curl_close($ch);

        if ($body === false) {
            return ['http' => 0, 'body' => self::redact($error, $spec['key']), 'curl_error' => true];
        }

        return ['http' => $http, 'body' => substr((string) $body, 0, self::KEEP_BYTES), 'curl_error' => false];
    }

    /** @param array{http:int,body:string,curl_error:bool} $response */
    private static function retryable(array $response): bool
    {
        return $response['curl_error'] || $response['http'] === 0
            || $response['http'] === 429 || $response['http'] >= 500;
    }

    /** @param array<string, mixed> $channel */
    private static function config(array $channel): array
    {
        if (isset($channel['probe_config']) && is_array($channel['probe_config'])) {
            return $channel['probe_config'];
        }

        $decoded = json_decode((string) ($channel['config'] ?? ''), true);

        return is_array($decoded) ? $decoded : [];
    }

    /** @param array<string, mixed> $channel */
    private static function upstreamModel(array $channel, string $model): string
    {
        $map = (array) (self::config($channel)['model_map'] ?? []);

        return isset($map[$model]) ? (string) $map[$model] : $model;
    }

    private static function summary(string $body, string $key): string
    {
        $summary = trim((string) preg_replace('/\s+/', ' ', self::redact($body, $key)));

        return mb_substr($summary, 0, 500);
    }

    private static function redact(string $value, string $key): string
    {
        return $key === '' ? $value : str_replace($key, '[REDACTED]', $value);
    }
}
