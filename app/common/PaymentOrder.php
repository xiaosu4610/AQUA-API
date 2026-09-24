<?php
/**
 * 充值订单
 *
 * 一条订单的生命周期只有三种状态：
 *
 *     pending ──(上游回调且验签通过)──> paid
 *        └────(站长手工置为失败 / 超时未付)──> failed
 *
 * ═══ 为什么订单要先落库、再跳转支付 ═══
 *
 * 顺序反了会有一个致命问题：用户付了钱、回调来了，
 * 我们却找不到这笔订单（因为订单只存在于跳转 URL 的参数里），
 * 结果就是「钱收了、余额没加」。所以必须**先把订单写进库**，
 * 再把订单号交给上游。
 *
 * ═══ 幂等是这条链路的核心 ═══
 *
 * 支付网关的回调**一定会重复**（没收到 success 就重试，这是规范行为）。
 * 因此到账动作必须是幂等的：用一个带条件的 UPDATE
 * （只有 pending 才能变成 paid）来保证「重复回调只加一次钱」。
 * 这是最容易出资金事故的地方，没有之一。
 */

declare(strict_types=1);

namespace app\common;

final class PaymentOrder
{
    public const STATUS_PENDING = 'pending';
    public const STATUS_PAID = 'paid';
    public const STATUS_FAILED = 'failed';

    /**
     * 创建订单（状态 pending）。
     *
     * @param float $amount 实付金额
     * @param float $credit 到账余额（= amount × 充值倍率）
     * @return int 订单 id
     */
    public static function create(
        int $userId,
        float $amount,
        float $credit,
        string $gateway,
        string $payType
    ): int {
        $now = time();

        Db::execute(
            'INSERT INTO payment_orders
                (order_no, user_id, amount, credit, gateway, pay_type, status, created_at)
             VALUES (?, ?, ?, ?, ?, ?, ?, ?)',
            [
                Epay::makeOrderNo($userId),
                $userId,
                self::dec($amount),
                self::dec($credit),
                mb_substr($gateway, 0, 16),
                mb_substr($payType, 0, 16),
                self::STATUS_PENDING,
                $now,
            ]
        );

        return (int) Db::pdo()->lastInsertId();
    }

    /** @return array<string, mixed>|null */
    public static function find(int $id): ?array
    {
        return Db::selectOne('SELECT * FROM payment_orders WHERE id = ?', [$id]);
    }

    /** @return array<string, mixed>|null */
    public static function findByOrderNo(string $orderNo): ?array
    {
        if ($orderNo === '') {
            return null;
        }

        return Db::selectOne('SELECT * FROM payment_orders WHERE order_no = ?', [$orderNo]);
    }

    /**
     * 把订单标记为已支付，并给用户加余额。
     *
     * ⚠️ 幂等的实现要点（这是资金安全的关键）：
     *   1. 先用**带条件的 UPDATE** 抢占「pending → paid」这个状态跃迁，
     *      受影响行数为 1 才算抢到。重复回调会在这一步返回 0，直接结束；
     *   2. **先抢占状态、再加钱**。顺序反了的话，如果加钱成功而状态更新失败
     *      （或两步之间进程被打断），重试就会重复加钱。
     *      现在的顺序最坏情况是「状态变了但钱没加」，那是可以人工补的，
     *      而重复加钱是不可逆的。
     *
     * @return array{ok:bool, already:bool, message:string, credit:float}
     */
    public static function markPaid(string $orderNo, string $tradeNo, string $rawNotify): array
    {
        $order = self::findByOrderNo($orderNo);

        if ($order === null) {
            return ['ok' => false, 'already' => false, 'message' => '订单不存在', 'credit' => 0.0];
        }

        // 抢占状态跃迁：只有 pending 能变成 paid
        $affected = Db::execute(
            'UPDATE payment_orders
             SET status = ?, trade_no = ?, notify_raw = ?, paid_at = ?
             WHERE order_no = ? AND status = ?',
            [
                self::STATUS_PAID,
                mb_substr($tradeNo, 0, 64),
                mb_substr($rawNotify, 0, 2000),
                time(),
                $orderNo,
                self::STATUS_PENDING,
            ]
        );

        if ($affected !== 1) {
            $current = (string) ($order['status'] ?? '');
            $fresh = self::findByOrderNo($orderNo);

            return [
                'ok' => $current === self::STATUS_PAID || ($fresh['status'] ?? '') === self::STATUS_PAID,
                'already' => true,
                'message' => '该订单已处理过（重复回调），未重复加钱',
                'credit' => 0.0,
            ];
        }

        // 状态已成功抢占，现在加钱
        $credit = (float) $order['credit'];
        User::credit((int) $order['user_id'], $credit);

        return [
            'ok' => true,
            'already' => false,
            'message' => '到账成功',
            'credit' => $credit,
        ];
    }

    /**
     * 标记订单失败（站长手工处理，或对账时发现异常）。
     */
    public static function markFailed(int $id, string $reason): void
    {
        Db::execute(
            'UPDATE payment_orders SET status = ?, notify_raw = ? WHERE id = ? AND status = ?',
            [self::STATUS_FAILED, mb_substr($reason, 0, 2000), $id, self::STATUS_PENDING]
        );
    }

    /**
     * 某用户的订单列表。
     *
     * @return array<int, array<string, mixed>>
     */
    public static function forUser(int $userId, int $limit = 50): array
    {
        return Db::select(
            'SELECT * FROM payment_orders WHERE user_id = ? ORDER BY id DESC LIMIT ' . max(1, min(200, $limit)),
            [$userId]
        );
    }

    /**
     * 后台订单分页（可按状态过滤）。
     *
     * @return array<int, array<string, mixed>>
     */
    public static function page(int $limit, int $offset, string $status = ''): array
    {
        $limit = max(1, min(200, $limit));

        if ($status === '') {
            return Db::select('SELECT * FROM payment_orders ORDER BY id DESC LIMIT ' . $limit . ' OFFSET ' . max(0, $offset));
        }

        return Db::select(
            'SELECT * FROM payment_orders WHERE status = ? ORDER BY id DESC LIMIT ' . $limit . ' OFFSET ' . max(0, $offset),
            [$status]
        );
    }

    public static function count(string $status = ''): int
    {
        $row = $status === ''
            ? Db::selectOne('SELECT COUNT(*) AS c FROM payment_orders')
            : Db::selectOne('SELECT COUNT(*) AS c FROM payment_orders WHERE status = ?', [$status]);

        return (int) ($row['c'] ?? 0);
    }

    /**
     * 收入汇总（后台看「一共收了多少钱」）。
     *
     * @return array{paid_count:int, paid_amount:float, pending_count:int}
     */
    public static function summary(): array
    {
        try {
            $row = Db::selectOne(
                'SELECT
                    SUM(CASE WHEN status = ? THEN 1 ELSE 0 END) AS paid_count,
                    SUM(CASE WHEN status = ? THEN amount ELSE 0 END) AS paid_amount,
                    SUM(CASE WHEN status = ? THEN 1 ELSE 0 END) AS pending_count
                 FROM payment_orders',
                [self::STATUS_PAID, self::STATUS_PAID, self::STATUS_PENDING]
            );
        } catch (\Throwable) {
            return ['paid_count' => 0, 'paid_amount' => 0.0, 'pending_count' => 0];
        }

        return [
            'paid_count' => (int) ($row['paid_count'] ?? 0),
            'paid_amount' => (float) ($row['paid_amount'] ?? 0),
            'pending_count' => (int) ($row['pending_count'] ?? 0),
        ];
    }

    private static function dec(float $value): string
    {
        return number_format($value, 10, '.', '');
    }
}
