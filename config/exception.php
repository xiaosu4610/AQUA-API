<?php
/**
 * This file is part of webman.
 *
 * Licensed under The MIT License
 * For full copyright and license information, please see the MIT-LICENSE.txt
 * Redistributions of files must retain the above copyright notice.
 *
 * @author    walkor<walkor@workerman.net>
 * @copyright walkor<walkor@workerman.net>
 * @link      http://www.workerman.net/
 * @license   http://www.opensource.org/licenses/mit-license.php MIT License
 */

return [
    // 换成项目自己的处理器：把未预期的异常渲染成统一错误页，
    // 并保留正确的 HTTP 状态码、对 /v1/* 回 JSON。
    // 详情见 app/exception/Handler.php
    '' => app\exception\Handler::class,
];