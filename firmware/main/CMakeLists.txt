idf_component_register(
    SRCS "main.c" "wifi.c" "display.c" "gfx.c" "frame.c" "audio.c" "vad.c"
         "head_touch.c" "power.c" "media.c" "ws_client.c" "ota_server.c"
    INCLUDE_DIRS "."
    PRIV_REQUIRES app_update esp_app_format esp_driver_touch_sens esp_event esp_http_client esp_http_server
                  esp_https_ota esp_lcd esp_netif esp_timer esp_wifi json mbedtls nvs_flash
)
target_compile_options(${COMPONENT_LIB} PRIVATE -Wall -Wextra -Werror=return-type -Wno-unused-parameter)
